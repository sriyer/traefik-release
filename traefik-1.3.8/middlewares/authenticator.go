package middlewares

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/abbot/go-http-auth"
	"github.com/codegangsta/negroni"
	"github.com/containous/traefik/log"
	"github.com/containous/traefik/types"
)

// Authenticator is a middleware that provides HTTP basic and digest authentication
type Authenticator struct {
	handler negroni.Handler
	users   map[string]string
}

// NewAuthenticator builds a new Autenticator given a config
func NewAuthenticator(authConfig *types.Auth) (*Authenticator, error) {
	if authConfig == nil {
		return nil, fmt.Errorf("Error creating Authenticator: auth is nil")
	}
	var err error
	authenticator := Authenticator{}
	if authConfig.Basic != nil {
		authenticator.users, err = parserBasicUsers(authConfig.Basic)
		if err != nil {
			return nil, err
		}
		basicAuth := auth.NewBasicAuthenticator("traefik", authenticator.secretBasic)
		authenticator.handler = negroni.HandlerFunc(func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
			if username := basicAuth.CheckAuth(r); username == "" {
				log.Debugf("Basic auth failed...")
				basicAuth.RequireAuth(w, r)
			} else {
				log.Debugf("Basic auth success...")
				if authConfig.HeaderField != "" {
					r.Header[authConfig.HeaderField] = []string{username}
				}
				next.ServeHTTP(w, r)
			}
		})
	} else if authConfig.Digest != nil {
		authenticator.users, err = parserDigestUsers(authConfig.Digest)
		if err != nil {
			return nil, err
		}
		digestAuth := auth.NewDigestAuthenticator("traefik", authenticator.secretDigest)
		authenticator.handler = negroni.HandlerFunc(func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
			if username, _ := digestAuth.CheckAuth(r); username == "" {
				log.Debugf("Digest auth failed...")
				digestAuth.RequireAuth(w, r)
			} else {
				log.Debugf("Digest auth success...")
				if authConfig.HeaderField != "" {
					r.Header[authConfig.HeaderField] = []string{username}
				}
				next.ServeHTTP(w, r)
			}
		})
	}
	return &authenticator, nil
}

func parserBasicUsers(basic *types.Basic) (map[string]string, error) {
	var userStrs []string
	if basic.UsersFile != "" {
		var err error
		if userStrs, err = getLinesFromFile(basic.UsersFile); err != nil {
			return nil, err
		}
	}
	userStrs = append(basic.Users, userStrs...)
	userMap := make(map[string]string)
	for _, user := range userStrs {
		split := strings.Split(user, ":")
		if len(split) != 2 {
			return nil, fmt.Errorf("Error parsing Authenticator user: %v", user)
		}
		userMap[split[0]] = split[1]
	}
	return userMap, nil
}

func parserDigestUsers(digest *types.Digest) (map[string]string, error) {
	var userStrs []string
	if digest.UsersFile != "" {
		var err error
		if userStrs, err = getLinesFromFile(digest.UsersFile); err != nil {
			return nil, err
		}
	}
	userStrs = append(digest.Users, userStrs...)
	userMap := make(map[string]string)
	for _, user := range userStrs {
		split := strings.Split(user, ":")
		if len(split) != 3 {
			return nil, fmt.Errorf("Error parsing Authenticator user: %v", user)
		}
		userMap[split[0]+":"+split[1]] = split[2]
	}
	return userMap, nil
}

func getLinesFromFile(filename string) ([]string, error) {
	dat, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	// Trim lines and filter out blanks
	rawLines := strings.Split(string(dat), "\n")
	var filteredLines []string
	for _, rawLine := range rawLines {
		line := strings.TrimSpace(rawLine)
		if line != "" {
			filteredLines = append(filteredLines, line)
		}
	}
	return filteredLines, nil
}

func (a *Authenticator) secretBasic(user, realm string) string {
	if secret, ok := a.users[user]; ok {
		return secret
	}
	log.Debugf("User not found: %s", user)
	return ""
}

func (a *Authenticator) secretDigest(user, realm string) string {
	if secret, ok := a.users[user+":"+realm]; ok {
		return secret
	}
	log.Debugf("User not found: %s:%s", user, realm)
	return ""
}

func (a *Authenticator) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	a.handler.ServeHTTP(rw, r, next)
}
