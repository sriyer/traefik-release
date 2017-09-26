package main

import (
	"github.com/go-check/check"
	"net/http"
	"os/exec"
	"time"

	checker "github.com/vdemeester/shakers"

	"crypto/tls"
	"errors"
	"fmt"
	"github.com/containous/traefik/integration/utils"
	"github.com/docker/libkv"
	"github.com/docker/libkv/store"
	"github.com/docker/libkv/store/etcd"
	"io/ioutil"
	"os"
	"strings"
)

// Etcd test suites (using libcompose)
type EtcdSuite struct {
	BaseSuite
	kv store.Store
}

func (s *EtcdSuite) SetUpTest(c *check.C) {
	s.createComposeProject(c, "etcd")
	s.composeProject.Start(c)

	etcd.Register()
	url := s.composeProject.Container(c, "etcd").NetworkSettings.IPAddress + ":2379"
	kv, err := libkv.NewStore(
		store.ETCD,
		[]string{url},
		&store.Config{
			ConnectionTimeout: 10 * time.Second,
		},
	)
	if err != nil {
		c.Fatal("Cannot create store etcd")
	}
	s.kv = kv

	// wait for etcd
	err = utils.Try(60*time.Second, func() error {
		_, err := kv.Exists("test")
		if err != nil {
			return fmt.Errorf("Etcd connection error to %s: %v", url, err)
		}
		return nil
	})
	c.Assert(err, checker.IsNil)
}

func (s *EtcdSuite) TearDownTest(c *check.C) {
	// shutdown and delete compose project
	if s.composeProject != nil {
		s.composeProject.Stop(c)
	}
}

func (s *EtcdSuite) TearDownSuite(c *check.C) {}

func (s *EtcdSuite) TestSimpleConfiguration(c *check.C) {
	etcdHost := s.composeProject.Container(c, "etcd").NetworkSettings.IPAddress
	file := s.adaptFile(c, "fixtures/etcd/simple.toml", struct{ EtcdHost string }{etcdHost})
	defer os.Remove(file)
	cmd := exec.Command(traefikBinary, "--configFile="+file)
	err := cmd.Start()
	c.Assert(err, checker.IsNil)
	defer cmd.Process.Kill()

	time.Sleep(1000 * time.Millisecond)
	// TODO validate : run on 80
	resp, err := http.Get("http://127.0.0.1:8000/")

	// Expected a 404 as we did not configure anything
	c.Assert(err, checker.IsNil)
	c.Assert(resp.StatusCode, checker.Equals, 404)
}

func (s *EtcdSuite) TestNominalConfiguration(c *check.C) {
	etcdHost := s.composeProject.Container(c, "etcd").NetworkSettings.IPAddress
	file := s.adaptFile(c, "fixtures/etcd/simple.toml", struct{ EtcdHost string }{etcdHost})
	defer os.Remove(file)
	cmd := exec.Command(traefikBinary, "--configFile="+file)
	err := cmd.Start()
	c.Assert(err, checker.IsNil)
	defer cmd.Process.Kill()

	whoami1 := s.composeProject.Container(c, "whoami1")
	whoami2 := s.composeProject.Container(c, "whoami2")
	whoami3 := s.composeProject.Container(c, "whoami3")
	whoami4 := s.composeProject.Container(c, "whoami4")

	backend1 := map[string]string{
		"/traefik/backends/backend1/circuitbreaker/expression": "NetworkErrorRatio() > 0.5",
		"/traefik/backends/backend1/servers/server1/url":       "http://" + whoami1.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend1/servers/server1/weight":    "10",
		"/traefik/backends/backend1/servers/server2/url":       "http://" + whoami2.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend1/servers/server2/weight":    "1",
	}
	backend2 := map[string]string{
		"/traefik/backends/backend2/loadbalancer/method":    "drr",
		"/traefik/backends/backend2/servers/server1/url":    "http://" + whoami3.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend2/servers/server1/weight": "1",
		"/traefik/backends/backend2/servers/server2/url":    "http://" + whoami4.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend2/servers/server2/weight": "2",
	}
	frontend1 := map[string]string{
		"/traefik/frontends/frontend1/backend":            "backend2",
		"/traefik/frontends/frontend1/entrypoints":        "http",
		"/traefik/frontends/frontend1/priority":           "1",
		"/traefik/frontends/frontend1/routes/test_1/rule": "Host:test.localhost",
	}
	frontend2 := map[string]string{
		"/traefik/frontends/frontend2/backend":            "backend1",
		"/traefik/frontends/frontend2/entrypoints":        "http",
		"/traefik/frontends/frontend2/priority":           "10",
		"/traefik/frontends/frontend2/routes/test_2/rule": "Path:/test",
	}
	for key, value := range backend1 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range backend2 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range frontend1 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range frontend2 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}

	// wait for etcd
	err = utils.Try(60*time.Second, func() error {
		_, err := s.kv.Exists("/traefik/frontends/frontend2/routes/test_2/rule")
		if err != nil {
			return err
		}
		return nil
	})
	c.Assert(err, checker.IsNil)

	// wait for traefik
	err = utils.TryRequest("http://127.0.0.1:8081/api/providers", 60*time.Second, func(res *http.Response) error {
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), "Path:/test") {
			return errors.New("Incorrect traefik config")
		}
		return nil
	})
	c.Assert(err, checker.IsNil)

	client := &http.Client{}
	req, err := http.NewRequest("GET", "http://127.0.0.1:8000/", nil)
	c.Assert(err, checker.IsNil)
	req.Host = "test.localhost"
	response, err := client.Do(req)

	c.Assert(err, checker.IsNil)
	c.Assert(response.StatusCode, checker.Equals, 200)

	body, err := ioutil.ReadAll(response.Body)
	c.Assert(err, checker.IsNil)
	if !strings.Contains(string(body), whoami3.NetworkSettings.IPAddress) &&
		!strings.Contains(string(body), whoami4.NetworkSettings.IPAddress) {
		c.Fail()
	}

	req, err = http.NewRequest("GET", "http://127.0.0.1:8000/test", nil)
	c.Assert(err, checker.IsNil)
	response, err = client.Do(req)

	c.Assert(err, checker.IsNil)
	c.Assert(response.StatusCode, checker.Equals, 200)

	body, err = ioutil.ReadAll(response.Body)
	c.Assert(err, checker.IsNil)
	if !strings.Contains(string(body), whoami1.NetworkSettings.IPAddress) &&
		!strings.Contains(string(body), whoami2.NetworkSettings.IPAddress) {
		c.Fail()
	}

	req, err = http.NewRequest("GET", "http://127.0.0.1:8000/test2", nil)
	req.Host = "test2.localhost"
	resp, err := client.Do(req)
	c.Assert(err, checker.IsNil)
	c.Assert(resp.StatusCode, checker.Equals, 404)

	req, err = http.NewRequest("GET", "http://127.0.0.1:8000/", nil)
	resp, err = client.Do(req)
	c.Assert(err, checker.IsNil)
	c.Assert(resp.StatusCode, checker.Equals, 404)
}

func (s *EtcdSuite) TestGlobalConfiguration(c *check.C) {
	etcdHost := s.composeProject.Container(c, "etcd").NetworkSettings.IPAddress
	err := s.kv.Put("/traefik/entrypoints/http/address", []byte(":8001"), nil)
	c.Assert(err, checker.IsNil)

	// wait for etcd
	err = utils.Try(60*time.Second, func() error {
		_, err := s.kv.Exists("/traefik/entrypoints/http/address")
		if err != nil {
			return err
		}
		return nil
	})
	c.Assert(err, checker.IsNil)

	// start traefik
	cmd := exec.Command(traefikBinary, "--configFile=fixtures/simple_web.toml", "--etcd", "--etcd.endpoint="+etcdHost+":4001")
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr

	err = cmd.Start()
	c.Assert(err, checker.IsNil)
	defer cmd.Process.Kill()

	whoami1 := s.composeProject.Container(c, "whoami1")
	whoami2 := s.composeProject.Container(c, "whoami2")
	whoami3 := s.composeProject.Container(c, "whoami3")
	whoami4 := s.composeProject.Container(c, "whoami4")

	backend1 := map[string]string{
		"/traefik/backends/backend1/circuitbreaker/expression": "NetworkErrorRatio() > 0.5",
		"/traefik/backends/backend1/servers/server1/url":       "http://" + whoami1.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend1/servers/server1/weight":    "10",
		"/traefik/backends/backend1/servers/server2/url":       "http://" + whoami2.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend1/servers/server2/weight":    "1",
	}
	backend2 := map[string]string{
		"/traefik/backends/backend2/loadbalancer/method":    "drr",
		"/traefik/backends/backend2/servers/server1/url":    "http://" + whoami3.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend2/servers/server1/weight": "1",
		"/traefik/backends/backend2/servers/server2/url":    "http://" + whoami4.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend2/servers/server2/weight": "2",
	}
	frontend1 := map[string]string{
		"/traefik/frontends/frontend1/backend":            "backend2",
		"/traefik/frontends/frontend1/entrypoints":        "http",
		"/traefik/frontends/frontend1/priority":           "1",
		"/traefik/frontends/frontend1/routes/test_1/rule": "Host:test.localhost",
	}
	frontend2 := map[string]string{
		"/traefik/frontends/frontend2/backend":            "backend1",
		"/traefik/frontends/frontend2/entrypoints":        "http",
		"/traefik/frontends/frontend2/priority":           "10",
		"/traefik/frontends/frontend2/routes/test_2/rule": "Path:/test",
	}
	for key, value := range backend1 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range backend2 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range frontend1 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range frontend2 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}

	// wait for etcd
	err = utils.Try(60*time.Second, func() error {
		_, err := s.kv.Exists("/traefik/frontends/frontend2/routes/test_2/rule")
		if err != nil {
			return err
		}
		return nil
	})
	c.Assert(err, checker.IsNil)

	// wait for traefik
	err = utils.TryRequest("http://127.0.0.1:8080/api/providers", 60*time.Second, func(res *http.Response) error {
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), "Path:/test") {
			return errors.New("Incorrect traefik config")
		}
		return nil
	})
	c.Assert(err, checker.IsNil)

	//check
	client := &http.Client{}
	req, err := http.NewRequest("GET", "http://127.0.0.1:8001/", nil)
	c.Assert(err, checker.IsNil)
	req.Host = "test.localhost"
	response, err := client.Do(req)

	c.Assert(err, checker.IsNil)
	c.Assert(response.StatusCode, checker.Equals, 200)
}

func (s *EtcdSuite) TestCertificatesContentstWithSNIConfigHandshake(c *check.C) {
	etcdHost := s.composeProject.Container(c, "etcd").NetworkSettings.IPAddress
	// start traefik
	cmd := exec.Command(traefikBinary, "--configFile=fixtures/simple_web.toml", "--etcd", "--etcd.endpoint="+etcdHost+":4001")
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr

	whoami1 := s.composeProject.Container(c, "whoami1")
	whoami2 := s.composeProject.Container(c, "whoami2")
	whoami3 := s.composeProject.Container(c, "whoami3")
	whoami4 := s.composeProject.Container(c, "whoami4")

	//Copy the contents of the certificate files into ETCD
	snitestComCert, err := ioutil.ReadFile("fixtures/https/snitest.com.cert")
	c.Assert(err, checker.IsNil)
	snitestComKey, err := ioutil.ReadFile("fixtures/https/snitest.com.key")
	c.Assert(err, checker.IsNil)
	snitestOrgCert, err := ioutil.ReadFile("fixtures/https/snitest.org.cert")
	c.Assert(err, checker.IsNil)
	snitestOrgKey, err := ioutil.ReadFile("fixtures/https/snitest.org.key")
	c.Assert(err, checker.IsNil)

	globalConfig := map[string]string{
		"/traefik/entrypoints/https/address":                     ":4443",
		"/traefik/entrypoints/https/tls/certificates/0/certfile": string(snitestComCert),
		"/traefik/entrypoints/https/tls/certificates/0/keyfile":  string(snitestComKey),
		"/traefik/entrypoints/https/tls/certificates/1/certfile": string(snitestOrgCert),
		"/traefik/entrypoints/https/tls/certificates/1/keyfile":  string(snitestOrgKey),
		"/traefik/defaultentrypoints/0":                          "https",
	}

	backend1 := map[string]string{
		"/traefik/backends/backend1/circuitbreaker/expression": "NetworkErrorRatio() > 0.5",
		"/traefik/backends/backend1/servers/server1/url":       "http://" + whoami1.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend1/servers/server1/weight":    "10",
		"/traefik/backends/backend1/servers/server2/url":       "http://" + whoami2.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend1/servers/server2/weight":    "1",
	}
	backend2 := map[string]string{
		"/traefik/backends/backend2/loadbalancer/method":    "drr",
		"/traefik/backends/backend2/servers/server1/url":    "http://" + whoami3.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend2/servers/server1/weight": "1",
		"/traefik/backends/backend2/servers/server2/url":    "http://" + whoami4.NetworkSettings.IPAddress + ":80",
		"/traefik/backends/backend2/servers/server2/weight": "2",
	}
	frontend1 := map[string]string{
		"/traefik/frontends/frontend1/backend":            "backend2",
		"/traefik/frontends/frontend1/entrypoints":        "http",
		"/traefik/frontends/frontend1/priority":           "1",
		"/traefik/frontends/frontend1/routes/test_1/rule": "Host:snitest.com",
	}
	frontend2 := map[string]string{
		"/traefik/frontends/frontend2/backend":            "backend1",
		"/traefik/frontends/frontend2/entrypoints":        "http",
		"/traefik/frontends/frontend2/priority":           "10",
		"/traefik/frontends/frontend2/routes/test_2/rule": "Host:snitest.org",
	}
	for key, value := range globalConfig {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range backend1 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range backend2 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range frontend1 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}
	for key, value := range frontend2 {
		err := s.kv.Put(key, []byte(value), nil)
		c.Assert(err, checker.IsNil)
	}

	// wait for etcd
	err = utils.Try(60*time.Second, func() error {
		_, err := s.kv.Exists("/traefik/frontends/frontend2/routes/test_2/rule")
		if err != nil {
			return err
		}
		return nil
	})
	c.Assert(err, checker.IsNil)

	err = cmd.Start()
	c.Assert(err, checker.IsNil)
	defer cmd.Process.Kill()

	// wait for traefik
	err = utils.TryRequest("http://127.0.0.1:8080/api/providers", 60*time.Second, func(res *http.Response) error {
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), "Host:snitest.org") {
			return errors.New("Incorrect traefik config")
		}
		return nil
	})
	c.Assert(err, checker.IsNil)

	//check
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "snitest.com",
	}
	conn, err := tls.Dial("tcp", "127.0.0.1:4443", tlsConfig)
	c.Assert(err, checker.IsNil, check.Commentf("failed to connect to server"))

	defer conn.Close()
	err = conn.Handshake()
	c.Assert(err, checker.IsNil, check.Commentf("TLS handshake error"))

	cs := conn.ConnectionState()
	err = cs.PeerCertificates[0].VerifyHostname("snitest.com")
	c.Assert(err, checker.IsNil, check.Commentf("certificate did not match SNI servername"))
}

func (s *EtcdSuite) TestCommandStoreConfig(c *check.C) {
	etcdHost := s.composeProject.Container(c, "etcd").NetworkSettings.IPAddress

	cmd := exec.Command(traefikBinary, "storeconfig", "--configFile=fixtures/simple_web.toml", "--etcd.endpoint="+etcdHost+":4001")
	err := cmd.Start()
	c.Assert(err, checker.IsNil)

	// wait for traefik finish without error
	cmd.Wait()

	//CHECK
	checkmap := map[string]string{
		"/traefik/loglevel":                 "DEBUG",
		"/traefik/defaultentrypoints/0":     "http",
		"/traefik/entrypoints/http/address": ":8000",
		"/traefik/web/address":              ":8080",
		"/traefik/etcd/endpoint":            (etcdHost + ":4001"),
	}

	for key, value := range checkmap {
		var p *store.KVPair
		err = utils.Try(60*time.Second, func() error {
			p, err = s.kv.Get(key)
			if err != nil {
				return err
			}
			return nil
		})
		c.Assert(err, checker.IsNil)

		c.Assert(string(p.Value), checker.Equals, value)

	}
}
