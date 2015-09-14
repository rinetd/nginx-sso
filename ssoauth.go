// ssoauth - Authenticate (verify) SSO cookie
//
// (c) 2015 by Johannes Gilger <heipei@hackvalue.de>
package main

import (
	"crypto"
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/heipei/nginx-sso/ssocookie"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// structs
// AclConfig maps from vhosts to structs with prefixes and permissions
type AclConfig map[string]struct {
	Users       []string `json:"Users"`
	Groups      []string `json:"Groups"`
	UrlPrefixes map[string]struct {
		Users  []string `json:"Users"`
		Groups []string `Groups:"Groups"`
	} `json:"UrlPrefixes"`
}

type Config struct {
	Cookie  string
	Port    int
	Headers struct {
		Ip  string
		Uri string
	}
	ReturnHeaders struct {
		User   string
		Groups string
		Expiry string
	}
	Pubkeyfile string
	Pubkey     crypto.PublicKey
	Acl        AclConfig
	Debug      bool
	Configfile string
}

// functions
func Unauthenticated(w http.ResponseWriter) {
	// Careful: StatusUnauthorized returns HTTP 401
	// HTTP 401 is called "Unauthorized", but actually means
	// "authentication failed" as per RFC 7235
	http.Error(w, "Not logged in", http.StatusUnauthorized)
}

func Unauthorized(w http.ResponseWriter) {
	// StatusForbidden returns HTTP 403
	// This means "authentication worked, but you don't have access to this
	// resource
	http.Error(w, "Access not granted", http.StatusForbidden)
}

func VerifyAcl(r *http.Request, config *Config, host string, uri string, sso_cookie *ssocookie.Cookie) bool {
	acl, ok := config.Acl[host]

	if !ok {
		return false
	}

	log.Debugf("acl entry: %s", acl)

	log.Debugf("vhosts: %s", acl.UrlPrefixes)
	for prefix, rules := range acl.UrlPrefixes {
		if strings.HasPrefix(uri, prefix) {
			log.Debugf("%s%s: Users: %s, Groups: %s\n", host, prefix, rules.Users, rules.Groups)
			for _, user := range rules.Users {
				if user == sso_cookie.P.U {
					log.Debugf("Found user %s\n", user)
					return true
				}
			}
			for _, group := range rules.Groups {
				for _, usergroup := range strings.Split(sso_cookie.P.G, ",") {
					if strings.HasPrefix(usergroup, group) {
						log.Debugf("Found group prefix %s\n", group)
						return true
					}
				}
			}
		}
	}
	return false
}

// Check that the cookie exists, is still valid and that the signature over the
// payload is OK
func CheckCookie(r *http.Request, config *Config, ip string, sso_cookie *ssocookie.Cookie) bool {
	cookie_string, err := r.Cookie(config.Cookie)
	if err != nil {
		log.Infof("No sso cookie from %s", ip)
		return false
	}

	json_string, _ := url.QueryUnescape(cookie_string.Value)
	log.Debugf("JSON payload from %s: %s", ip, json_string)

	err = json.Unmarshal([]byte(json_string), &sso_cookie)
	if err != nil {
		log.Errorf("Error unmarshaling JSON: %s\n", err)
		return false
	}

	// Verify that the signature of the cookie is correct
	if !ssocookie.VerifyCookie(ip, sso_cookie,
		config.Pubkey.(*ecdsa.PublicKey)) {
		return false
	}

	return true
}

func AuthHandler(config *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// Get the relevant HTTP headers
		uri := r.Header.Get(config.Headers.Uri)
		ip := r.Header.Get(config.Headers.Ip)
		host := r.Host

		if ip == "" {
			log.Warnf("Header %s missing", config.Headers.Ip)
			Unauthenticated(w)
			return
		}

		if uri == "" {
			log.Warnf("Header %s missing", config.Headers.Uri)
			Unauthenticated(w)
			return
		}

		requestLogger := log.WithFields(log.Fields{
			"ip":   ip,
			"uri":  uri,
			"host": host,
		})
		// Print remote address and UTC-adjusted timestamp in RFC3339
		// RFC3339 is a profile of ISO 8601
		requestLogger.Infof("Request at %s", time.Now().UTC().Format(time.RFC3339))

		// Populate and check the SSO cookie
		sso_cookie := new(ssocookie.Cookie)
		cookie_ok := CheckCookie(r, config, ip, sso_cookie)

		if !cookie_ok {
			requestLogger.Warnf("Cookie not OK")
			Unauthenticated(w)
			return
		}

		// Look for ACL entry for this host, URI, user and groups
		acl_ok := VerifyAcl(r, config, host, uri, sso_cookie)

		if !acl_ok {
			requestLogger.Warnf("Found no ACL entry for user %s, groups %s", sso_cookie.P.U, sso_cookie.P.G)
			Unauthorized(w)
			return
		}

		// We arrived here, accept the request and set the reply headers
		w.Header().Set(config.ReturnHeaders.User, sso_cookie.P.U)
		w.Header().Set(config.ReturnHeaders.Groups, sso_cookie.P.G)
		w.Header().Set(config.ReturnHeaders.Expiry, fmt.Sprintf("%d", sso_cookie.E))
		fmt.Fprintf(w, "Authorized!\n")

		requestLogger.Infof("Succesful request by %s", sso_cookie.P.U)
		return
	})
}

func CheckError(e error) {
	if e != nil {
		log.Fatal(e)
		panic(e)
	}
}

func ReadConfig(config *Config, configfile string) {
	// Read the config file
	c, err := ioutil.ReadFile(configfile)
	if err != nil {
		if config.Configfile == "" {
			log.Fatal(err)
			panic(err)
		} else {
			log.Errorf("Reloading config file failed: %s", err)
			return
		}
	}

	// Unmarshal the config file
	err = json.Unmarshal(c, &config)
	if err != nil {
		if config.Configfile == "" {
			log.Fatal(err)
			panic(err)
		} else {
			log.Errorf("Reloading config file failed: %s", err)
			return
		}
	}

	// Set appropriate log-level
	if config.Debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	config.Configfile = configfile
	config.Pubkey, err = ssocookie.ReadECCPublicKeyPem(config.Pubkeyfile, config.Pubkey)
	if err != nil {
		if config.Configfile == "" {
			log.Fatal(err)
			panic(err)
		} else {
			log.Errorf("Reloading config file failed: %s", err)
			return
		}
	}
}

func ParseArgs(config *Config) {
	configfile := flag.String("config", "config.json", "ACL config file (JSON)")
	flag.BoolVar(&config.Debug, "debug", false, "Debug-level output")
	flag.Parse()

	// Read the config file
	ReadConfig(config, *configfile)
}

func main() {
	log.Infof("ssoauth starting")
	config := new(Config)

	http.Handle("/auth", AuthHandler(config))

	ParseArgs(config)

	// Setup signal handling
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)

	go func() {
		for sig := range c {
			log.Warnf("Received SIGHUP (%s), reloading config...", sig)
			ReadConfig(config, config.Configfile)
		}
	}()

	log.Infof("ssoauth server running on 127.0.0.1:%d", config.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", config.Port), nil))
}
