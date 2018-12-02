/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package exec

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/golang/glog"
	"golang.org/x/crypto/ssh/terminal"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	"k8s.io/client-go/pkg/apis/clientauthentication/v1alpha1"
	"k8s.io/client-go/tools/clientcmd/api"
)

const execInfoEnv = "KUBERNETES_EXEC_INFO"

var scheme = runtime.NewScheme()
var codecs = serializer.NewCodecFactory(scheme)

func init() {
	v1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	v1alpha1.AddToScheme(scheme)
	clientauthentication.AddToScheme(scheme)
}

var (
	// Since transports can be constantly re-initialized by programs like kubectl,
	// keep a cache of initialized authenticators keyed by a hash of their config.
	globalCache = newCache()
	// The list of API versions we accept.
	apiVersions = map[string]schema.GroupVersion{
		v1alpha1.SchemeGroupVersion.String(): v1alpha1.SchemeGroupVersion,
	}
)

func newCache() *cache {
	return &cache{m: make(map[string]*Authenticator)}
}

func cacheKey(c *api.ExecConfig) string {
	return fmt.Sprintf("%#v", c)
}

type cache struct {
	mu sync.Mutex
	m  map[string]*Authenticator
}

func (c *cache) get(s string) (*Authenticator, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.m[s]
	return a, ok
}

// put inserts an authenticator into the cache. If an authenticator is already
// associated with the key, the first one is returned instead.
func (c *cache) put(s string, a *Authenticator) *Authenticator {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.m[s]
	if ok {
		return existing
	}
	c.m[s] = a
	return a
}

// GetAuthenticator returns an exec-based plugin for providing client credentials.
func GetAuthenticator(config *api.ExecConfig) (*Authenticator, error) {
	return newAuthenticator(globalCache, config)
}

func newAuthenticator(c *cache, config *api.ExecConfig) (*Authenticator, error) {
	key := cacheKey(config)
	if a, ok := c.get(key); ok {
		return a, nil
	}

	gv, ok := apiVersions[config.APIVersion]
	if !ok {
		return nil, fmt.Errorf("exec plugin: invalid apiVersion %q", config.APIVersion)
	}

	a := &Authenticator{
		cmd:   config.Command,
		args:  config.Args,
		group: gv,

		stdin:       os.Stdin,
		stderr:      os.Stderr,
		interactive: terminal.IsTerminal(int(os.Stdout.Fd())),
		now:         time.Now,
		environ:     os.Environ,
	}

	for _, env := range config.Env {
		a.env = append(a.env, env.Name+"="+env.Value)
	}

	return c.put(key, a), nil
}

// Authenticator is a client credential provider that rotates credentials by executing a plugin.
// The plugin input and output are defined by the API group client.authentication.k8s.io.
type Authenticator struct {
	// Set by the config
	cmd   string
	args  []string
	group schema.GroupVersion
	env   []string

	// Stubbable for testing
	stdin       io.Reader
	stderr      io.Writer
	interactive bool
	now         func() time.Time
	environ     func() []string

	// Cached results.
	//
	// The mutex also guards calling the plugin. Since the plugin could be
	// interactive we want to make sure it's only called once.
	mu          sync.Mutex
	cachedToken string
	exp         time.Time
}

// WrapTransport instruments an existing http.RoundTripper with credentials returned
// by the plugin.
func (a *Authenticator) WrapTransport(rt http.RoundTripper) http.RoundTripper {
	return &roundTripper{a, rt}
}

type roundTripper struct {
	a    *Authenticator
	base http.RoundTripper
}

func (r *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// If a user has already set credentials, use that. This makes commands like
	// "kubectl get --token (token) pods" work.
	if req.Header.Get("Authorization") != "" {
		return r.base.RoundTrip(req)
	}

	token, err := r.a.token()
	if err != nil {
		return nil, fmt.Errorf("getting token: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusUnauthorized {
		resp := &clientauthentication.Response{
			Header: res.Header,
			Code:   int32(res.StatusCode),
		}
		if err := r.a.refresh(token, resp); err != nil {
			glog.Errorf("refreshing token: %v", err)
		}
	}
	return res, nil
}

func (a *Authenticator) tokenExpired() bool {
	if a.exp.IsZero() {
		return false
	}
	return a.now().After(a.exp)
}

func (a *Authenticator) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cachedToken != "" && !a.tokenExpired() {
		return a.cachedToken, nil
	}

	return a.getToken(nil)
}

// refresh executes the plugin to force a rotation of the token.
func (a *Authenticator) refresh(token string, r *clientauthentication.Response) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if token != a.cachedToken {
		// Token already rotated.
		return nil
	}

	_, err := a.getToken(r)
	return err
}

// getToken executes the plugin and reads the credentials from stdout. It must be
// called while holding the Authenticator's mutex.
func (a *Authenticator) getToken(r *clientauthentication.Response) (string, error) {
	cred := &clientauthentication.ExecCredential{
		Spec: clientauthentication.ExecCredentialSpec{
			Response:    r,
			Interactive: a.interactive,
		},
	}

	data, err := runtime.Encode(codecs.LegacyCodec(a.group), cred)
	if err != nil {
		return "", fmt.Errorf("encode ExecCredentials: %v", err)
	}

	env := append(a.environ(), a.env...)
	env = append(env, fmt.Sprintf("%s=%s", execInfoEnv, data))

	stdout := &bytes.Buffer{}
	cmd := exec.Command(a.cmd, a.args...)
	cmd.Env = env
	cmd.Stderr = a.stderr
	cmd.Stdout = stdout
	if a.interactive {
		cmd.Stdin = a.stdin
	}

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("exec: %v", err)
	}

	_, gvk, err := codecs.UniversalDecoder(a.group).Decode(stdout.Bytes(), nil, cred)
	if err != nil {
		return "", fmt.Errorf("decode stdout: %v", err)
	}
	if gvk.Group != a.group.Group || gvk.Version != a.group.Version {
		return "", fmt.Errorf("exec plugin is configured to use API version %s, plugin returned version %s",
			a.group, schema.GroupVersion{Group: gvk.Group, Version: gvk.Version})
	}

	if cred.Status == nil {
		return "", fmt.Errorf("exec plugin didn't return a status field")
	}
	if cred.Status.Token == "" {
		return "", fmt.Errorf("exec plugin didn't return a token")
	}

	if cred.Status.ExpirationTimestamp != nil {
		a.exp = cred.Status.ExpirationTimestamp.Time
	} else {
		a.exp = time.Time{}
	}
	a.cachedToken = cred.Status.Token

	return a.cachedToken, nil
}
