// Package deploy holds static checks on the deployment configuration.
//
// These are cheap assertions about files that are otherwise only exercised by
// actually running the stack, which no unit test does. They exist because the
// failures they guard against are silent, look like application bugs, and cost
// an afternoon to diagnose.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readNginxConf(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "nginx.conf")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// literalUpstream matches a proxy_pass whose host is spelled out rather than
// named through a variable.
var literalUpstream = regexp.MustCompile(`proxy_pass\s+https?://[a-zA-Z0-9]`)

// TestProxyPassResolvesAtRequestTime is the regression guard for a 502 that is
// entirely invisible from the application side.
//
// nginx resolves a literal hostname in proxy_pass exactly once, when it parses
// its configuration, and caches that address for the process's lifetime.
// Container addresses are not stable: rebuilding, restarting or crashing gives
// the API a new one, and Docker hands the old one to whichever container asks
// next. In this stack that is usually the worker, which serves 8081 and not
// 8080, so every /api request fails with "connection refused" while the API sits
// there perfectly healthy — and it stays broken until nginx itself is restarted.
//
// Naming the upstream through a variable defers resolution to request time and
// makes the proxy self-healing.
func TestProxyPassResolvesAtRequestTime(t *testing.T) {
	conf := readNginxConf(t)

	for i, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "proxy_pass") {
			continue
		}
		if literalUpstream.MatchString(trimmed) {
			t.Errorf("deploy/nginx.conf:%d uses a literal upstream host:\n\t%s\n"+
				"nginx caches that address at startup, so the proxy breaks permanently the next "+
				"time the API container is recreated. Name the upstream through a variable "+
				"(proxy_pass http://$encore_api:8080...) so it is resolved per request.", i+1, trimmed)
		}
	}
}

// TestResolverIsConfigured checks the other half of the fix: a variable upstream
// is only resolvable if nginx has been told which DNS server to ask.
func TestResolverIsConfigured(t *testing.T) {
	conf := readNginxConf(t)

	if !strings.Contains(conf, "resolver 127.0.0.11") {
		t.Error("deploy/nginx.conf must set `resolver 127.0.0.11`, Docker's embedded DNS server on " +
			"the user-defined network Compose creates. Without it, a variable upstream cannot be resolved " +
			"at all and every proxied request fails.")
	}
	if !strings.Contains(conf, "ipv6=off") {
		t.Error("the resolver should set ipv6=off: Docker's embedded DNS answers AAAA queries that " +
			"then fail to connect, which shows up as intermittent 502s.")
	}
}

// TestVariableProxyPassCarriesTheURI guards the footgun that comes with the fix.
//
// A proxy_pass containing a variable loses nginx's implicit URI forwarding, so
// the upstream URI has to be stated. Forgetting it on the /api/ prefix location
// sends every request to the upstream's root, which the API answers with 404 —
// a much more confusing symptom than a 502.
func TestVariableProxyPassCarriesTheURI(t *testing.T) {
	conf := readNginxConf(t)

	var inAPILocation bool
	var depth int
	for i, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "location /api/") {
			inAPILocation, depth = true, 0
		}
		if !inAPILocation {
			continue
		}
		depth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
		if strings.HasPrefix(trimmed, "proxy_pass") && !strings.HasPrefix(trimmed, "#") {
			if !strings.Contains(trimmed, "$request_uri") {
				t.Fatalf("deploy/nginx.conf:%d proxies the /api/ prefix without $request_uri:\n\t%s\n"+
					"A proxy_pass with a variable does not forward the original URI by itself, so every "+
					"API call would reach the upstream's root and come back 404.", i+1, trimmed)
			}
			return
		}
		if depth < 0 {
			inAPILocation = false
		}
	}
	t.Fatal("deploy/nginx.conf has no proxy_pass inside `location /api/`; the web container would " +
		"serve the single-page app for API calls instead of proxying them")
}

// TestAPIAndWorkerPortsDoNotOverlap records why the original failure was so
// confusing: the stale address belonged to the worker, which listens on a
// different port, so the connection was refused outright rather than serving
// something recognisably wrong.
func TestAPIAndWorkerPortsDoNotOverlap(t *testing.T) {
	compose, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	if !strings.Contains(string(compose), `ENCORE_HTTP_ADDR: ":8081"`) {
		t.Error("the worker should serve its health and metrics listener on 8081, distinct from the " +
			"API's 8080, so the two are never confused for one another on a shared network")
	}
}
