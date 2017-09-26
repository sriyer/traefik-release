package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	fmtlog "log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/containous/flaeg"
	"github.com/containous/staert"
	"github.com/containous/traefik/acme"
	"github.com/containous/traefik/cluster"
	"github.com/containous/traefik/log"
	"github.com/containous/traefik/middlewares"
	"github.com/containous/traefik/provider/kubernetes"
	"github.com/containous/traefik/safe"
	"github.com/containous/traefik/server"
	"github.com/containous/traefik/types"
	"github.com/containous/traefik/version"
	"github.com/coreos/go-systemd/daemon"
	"github.com/docker/libkv/store"
	"github.com/satori/go.uuid"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	//traefik config inits
	traefikConfiguration := server.NewTraefikConfiguration()
	traefikPointersConfiguration := server.NewTraefikDefaultPointersConfiguration()
	//traefik Command init
	traefikCmd := &flaeg.Command{
		Name: "traefik",
		Description: `traefik is a modern HTTP reverse proxy and load balancer made to deploy microservices with ease.
Complete documentation is available at https://traefik.io`,
		Config:                traefikConfiguration,
		DefaultPointersConfig: traefikPointersConfiguration,
		Run: func() error {
			run(traefikConfiguration)
			return nil
		},
	}

	//storeconfig Command init
	var kv *staert.KvSource
	var err error

	storeconfigCmd := &flaeg.Command{
		Name:                  "storeconfig",
		Description:           `Store the static traefik configuration into a Key-value stores. Traefik will not start.`,
		Config:                traefikConfiguration,
		DefaultPointersConfig: traefikPointersConfiguration,
		Run: func() error {
			if kv == nil {
				return fmt.Errorf("Error using command storeconfig, no Key-value store defined")
			}
			jsonConf, err := json.Marshal(traefikConfiguration.GlobalConfiguration)
			if err != nil {
				return err
			}
			fmtlog.Printf("Storing configuration: %s\n", jsonConf)
			err = kv.StoreConfig(traefikConfiguration.GlobalConfiguration)
			if err != nil {
				return err
			}
			if traefikConfiguration.GlobalConfiguration.ACME != nil && len(traefikConfiguration.GlobalConfiguration.ACME.StorageFile) > 0 {
				// convert ACME json file to KV store
				store := acme.NewLocalStore(traefikConfiguration.GlobalConfiguration.ACME.StorageFile)
				object, err := store.Load()
				if err != nil {
					return err
				}
				meta := cluster.NewMetadata(object)
				err = meta.Marshall()
				if err != nil {
					return err
				}
				source := staert.KvSource{
					Store:  kv,
					Prefix: traefikConfiguration.GlobalConfiguration.ACME.Storage,
				}
				err = source.StoreConfig(meta)
				if err != nil {
					return err
				}
			}
			return nil
		},
		Metadata: map[string]string{
			"parseAllSources": "true",
		},
	}

	//init flaeg source
	f := flaeg.New(traefikCmd, os.Args[1:])
	//add custom parsers
	f.AddParser(reflect.TypeOf(server.EntryPoints{}), &server.EntryPoints{})
	f.AddParser(reflect.TypeOf(server.DefaultEntryPoints{}), &server.DefaultEntryPoints{})
	f.AddParser(reflect.TypeOf(types.Constraints{}), &types.Constraints{})
	f.AddParser(reflect.TypeOf(kubernetes.Namespaces{}), &kubernetes.Namespaces{})
	f.AddParser(reflect.TypeOf([]acme.Domain{}), &acme.Domains{})
	f.AddParser(reflect.TypeOf(types.Buckets{}), &types.Buckets{})

	//add commands
	f.AddCommand(newVersionCmd())
	f.AddCommand(newBugCmd(traefikConfiguration, traefikPointersConfiguration))
	f.AddCommand(storeconfigCmd)

	usedCmd, err := f.GetCommand()
	if err != nil {
		fmtlog.Println(err)
		os.Exit(-1)
	}

	if _, err := f.Parse(usedCmd); err != nil {
		fmtlog.Printf("Error parsing command: %s\n", err)
		os.Exit(-1)
	}

	//staert init
	s := staert.NewStaert(traefikCmd)
	//init toml source
	toml := staert.NewTomlSource("traefik", []string{traefikConfiguration.ConfigFile, "/etc/traefik/", "$HOME/.traefik/", "."})

	//add sources to staert
	s.AddSource(toml)
	s.AddSource(f)
	if _, err := s.LoadConfig(); err != nil {
		fmtlog.Println(fmt.Errorf("Error reading TOML config file %s : %s", toml.ConfigFileUsed(), err))
		os.Exit(-1)
	}

	traefikConfiguration.ConfigFile = toml.ConfigFileUsed()

	kv, err = CreateKvSource(traefikConfiguration)
	if err != nil {
		fmtlog.Printf("Error creating kv store: %s\n", err)
		os.Exit(-1)
	}

	// IF a KV Store is enable and no sub-command called in args
	if kv != nil && usedCmd == traefikCmd {
		if traefikConfiguration.Cluster == nil {
			traefikConfiguration.Cluster = &types.Cluster{Node: uuid.NewV4().String()}
		}
		if traefikConfiguration.Cluster.Store == nil {
			traefikConfiguration.Cluster.Store = &types.Store{Prefix: kv.Prefix, Store: kv.Store}
		}
		s.AddSource(kv)
		if _, err := s.LoadConfig(); err != nil {
			fmtlog.Printf("Error loading configuration: %s\n", err)
			os.Exit(-1)
		}
	}

	if err := s.Run(); err != nil {
		fmtlog.Printf("Error running traefik: %s\n", err)
		os.Exit(-1)
	}

	os.Exit(0)
}

func run(traefikConfiguration *server.TraefikConfiguration) {
	fmtlog.SetFlags(fmtlog.Lshortfile | fmtlog.LstdFlags)

	// load global configuration
	globalConfiguration := traefikConfiguration.GlobalConfiguration

	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = globalConfiguration.MaxIdleConnsPerHost
	if globalConfiguration.InsecureSkipVerify {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	loggerMiddleware := middlewares.NewLogger(globalConfiguration.AccessLogsFile)
	defer loggerMiddleware.Close()

	if globalConfiguration.File != nil && len(globalConfiguration.File.Filename) == 0 {
		// no filename, setting to global config file
		if len(traefikConfiguration.ConfigFile) != 0 {
			globalConfiguration.File.Filename = traefikConfiguration.ConfigFile
		} else {
			log.Errorln("Error using file configuration backend, no filename defined")
		}
	}

	if len(globalConfiguration.EntryPoints) == 0 {
		globalConfiguration.EntryPoints = map[string]*server.EntryPoint{"http": {Address: ":80"}}
		globalConfiguration.DefaultEntryPoints = []string{"http"}
	}

	if globalConfiguration.Debug {
		globalConfiguration.LogLevel = "DEBUG"
	}

	// logging
	level, err := logrus.ParseLevel(strings.ToLower(globalConfiguration.LogLevel))
	if err != nil {
		log.Error("Error getting level", err)
	}
	log.SetLevel(level)
	if len(globalConfiguration.TraefikLogsFile) > 0 {
		dir := filepath.Dir(globalConfiguration.TraefikLogsFile)

		err := os.MkdirAll(dir, 0755)
		if err != nil {
			log.Errorf("Failed to create log path %s: %s", dir, err)
		}

		fi, err := os.OpenFile(globalConfiguration.TraefikLogsFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		defer func() {
			if err := fi.Close(); err != nil {
				log.Error("Error closing file", err)
			}
		}()
		if err != nil {
			log.Error("Error opening file", err)
		} else {
			log.SetOutput(fi)
			log.SetFormatter(&logrus.TextFormatter{DisableColors: true, FullTimestamp: true, DisableSorting: true})
		}
	} else {
		log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true, DisableSorting: true})
	}
	jsonConf, _ := json.Marshal(globalConfiguration)
	log.Infof("Traefik version %s built on %s", version.Version, version.BuildDate)

	if globalConfiguration.CheckNewVersion {
		ticker := time.NewTicker(24 * time.Hour)
		safe.Go(func() {
			version.CheckNewVersion()
			for {
				select {
				case <-ticker.C:
					version.CheckNewVersion()
				}
			}
		})
	}

	if len(traefikConfiguration.ConfigFile) != 0 {
		log.Infof("Using TOML configuration file %s", traefikConfiguration.ConfigFile)
	}
	log.Debugf("Global configuration loaded %s", string(jsonConf))
	svr := server.NewServer(globalConfiguration)
	svr.Start()
	defer svr.Close()
	sent, err := daemon.SdNotify(false, "READY=1")
	if !sent && err != nil {
		log.Error("Fail to notify", err)
	}
	t, err := daemon.SdWatchdogEnabled(false)
	if err != nil {
		log.Error("Problem with watchdog", err)
	} else if t != 0 {
		// Send a ping each half time given
		t = t / 2
		log.Info("Watchdog activated with timer each ", t)
		safe.Go(func() {
			tick := time.Tick(t)
			for range tick {
				if ok, _ := daemon.SdNotify(false, "WATCHDOG=1"); !ok {
					log.Error("Fail to tick watchdog")
				}
			}
		})
	}
	svr.Wait()
	log.Info("Shutting down")
}

// CreateKvSource creates KvSource
// TLS support is enable for Consul and Etcd backends
func CreateKvSource(traefikConfiguration *server.TraefikConfiguration) (*staert.KvSource, error) {
	var kv *staert.KvSource
	var store store.Store
	var err error

	switch {
	case traefikConfiguration.Consul != nil:
		store, err = traefikConfiguration.Consul.CreateStore()
		kv = &staert.KvSource{
			Store:  store,
			Prefix: traefikConfiguration.Consul.Prefix,
		}
	case traefikConfiguration.Etcd != nil:
		store, err = traefikConfiguration.Etcd.CreateStore()
		kv = &staert.KvSource{
			Store:  store,
			Prefix: traefikConfiguration.Etcd.Prefix,
		}
	case traefikConfiguration.Zookeeper != nil:
		store, err = traefikConfiguration.Zookeeper.CreateStore()
		kv = &staert.KvSource{
			Store:  store,
			Prefix: traefikConfiguration.Zookeeper.Prefix,
		}
	case traefikConfiguration.Boltdb != nil:
		store, err = traefikConfiguration.Boltdb.CreateStore()
		kv = &staert.KvSource{
			Store:  store,
			Prefix: traefikConfiguration.Boltdb.Prefix,
		}
	}
	return kv, err
}
