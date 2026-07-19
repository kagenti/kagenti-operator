package keycloak

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func inSyncClientRep() map[string]interface{} {
	return map[string]interface{}{
		"id":                        "uuid-1",
		"clientId":                  "ns/workload",
		"name":                      "ns/workload",
		"standardFlowEnabled":       true,
		"directAccessGrantsEnabled": true,
		"serviceAccountsEnabled":    true,
		"fullScopeAllowed":          false,
		"publicClient":              false,
		"clientAuthenticatorType":   "client-secret",
		"attributes":                map[string]interface{}{"standard.token.exchange.enabled": []interface{}{"false"}},
	}
}

func TestAdmin_RegisterOrFetchClient(t *testing.T) {
	var tokenCalls, listCalls, getCalls, createCalls, putCalls, secretCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == testMasterRealmTokenPath:
			tokenCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t"})
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients") && r.Method == http.MethodGet && r.URL.Query().Get("clientId") != "":
			listCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "uuid-1", "clientId": "ns/workload"}})
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients/") && strings.HasSuffix(r.URL.Path, "/client-secret"):
			secretCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"value": "topsecret"})
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients/") && r.Method == http.MethodGet:
			getCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(inSyncClientRep())
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients/") && r.Method == http.MethodPut:
			putCalls++
			t.Fatal("unexpected PUT when client is in sync")
		case r.URL.Path == "/admin/realms/rossoctl/clients" && r.Method == http.MethodPost:
			createCalls++
			t.Fatal("unexpected create when client exists")
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	id, sec, err := a.RegisterOrFetchClient(context.Background(), "admin", "pw", ClientRegistrationParams{
		Realm:      "rossoctl",
		ClientID:   "ns/workload",
		ClientName: "ns/workload",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "uuid-1" || sec != "topsecret" {
		t.Fatalf("got id=%q sec=%q", id, sec)
	}
	if tokenCalls != 1 || listCalls != 1 || getCalls != 1 || secretCalls != 1 || putCalls != 0 {
		t.Fatalf("calls token=%d list=%d get=%d put=%d create=%d secret=%d", tokenCalls, listCalls, getCalls, putCalls, createCalls, secretCalls)
	}
}

func TestAdmin_RegisterOrFetchClient_updatesDrift(t *testing.T) {
	var getCalls, putCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t"})
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients") && r.Method == http.MethodGet && r.URL.Query().Get("clientId") != "":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "uuid-1", "clientId": "ns/workload"}})
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients/") && strings.HasSuffix(r.URL.Path, "/client-secret"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"value": "topsecret"})
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients/") && r.Method == http.MethodGet:
			getCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(inSyncClientRep())
		case strings.HasPrefix(r.URL.Path, "/admin/realms/rossoctl/clients/") && r.Method == http.MethodPut:
			putCalls++
			if r.URL.Path != "/admin/realms/rossoctl/clients/uuid-1" {
				t.Fatalf("unexpected PUT path %s", r.URL.Path)
			}
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			attrs, _ := body["attributes"].(map[string]interface{})
			if attrs == nil {
				t.Fatal("expected attributes in PUT body")
			}
			ex, _ := attrs["standard.token.exchange.enabled"].(string)
			if ex != "true" {
				t.Fatalf("expected token exchange true in PUT, got %#v", attrs["standard.token.exchange.enabled"])
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/admin/realms/rossoctl/clients" && r.Method == http.MethodPost:
			t.Fatal("unexpected create")
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, _, err := a.RegisterOrFetchClient(context.Background(), "admin", "pw", ClientRegistrationParams{
		Realm:               "rossoctl",
		ClientID:            "ns/workload",
		ClientName:          "ns/workload",
		TokenExchangeEnable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if getCalls != 1 || putCalls != 1 {
		t.Fatalf("expected 1 get and 1 put, got get=%d put=%d", getCalls, putCalls)
	}
}

func TestNewHTTPClient_defaults(t *testing.T) {
	c := NewHTTPClient(HTTPClientConfig{})
	if c.Timeout != 60*time.Second || c.Transport != nil {
		t.Fatalf("expected 60s timeout and nil transport, got timeout=%v transport=%v", c.Timeout, c.Transport)
	}
}

func TestNewHTTPClient_TLSConfig(t *testing.T) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	c := NewHTTPClient(HTTPClientConfig{TLSConfig: tlsCfg})
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil {
		t.Fatalf("expected *http.Transport with TLSClientConfig, got %T", c.Transport)
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion: got %v want %v", tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
}

func TestNewHTTPClient_customTransport(t *testing.T) {
	custom := http.DefaultTransport
	c := NewHTTPClient(HTTPClientConfig{Transport: custom, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}})
	if c.Transport != custom {
		t.Fatal("expected custom transport to be used as-is")
	}
}
