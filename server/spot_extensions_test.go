package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDelegatedSessionContextIsPrivateAndHostBound(t *testing.T) {
	login := newTestDelegatedLogin(t)
	identity := Identity{Email: "viewer@example.com", Context: map[string]json.RawMessage{
		"example": json.RawMessage(`{"opaque":"issuer-private-proof"}`),
	}}
	claims := testLoginClaims("demo.sites.localhost", "context-proof", time.Now())
	claims["email"], claims["context"] = identity.Email, identity.Context
	verified, err := login.verifyLoginToken(signTestLoginToken(t, testLoginTokenSecret, nil, claims), "demo.sites.localhost", testLoginState)
	if err != nil || string(verified.Context["example"]) != string(identity.Context["example"]) {
		t.Fatalf("login token context lost: %v", err)
	}
	value, err := login.signSession(verified, "demo.sites.localhost")
	if err != nil {
		t.Fatal(err)
	}
	actual, _, ok := login.verifySession(value, "demo.sites.localhost")
	if !ok || string(actual.Context["example"]) != string(identity.Context["example"]) {
		t.Fatal("delegated session context lost")
	}
	raw, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "issuer-private-proof") {
		t.Fatal("public identity exposes session context")
	}
	if _, _, ok := login.verifySession(value, "other.sites.localhost"); ok {
		t.Fatal("accepted another host")
	}
}

func TestExtensionSettingsSurvivePolicyRoundTrip(t *testing.T) {
	policy, err := parseAccessPolicy("demo", []byte(`{"allow":["viewer@example.com"],"download":false,"extensions":{"example":{"enabled":false,"nested":{"value":42}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	clone := cloneAccessPolicy(policy)
	clone.Extensions["example"][0] = ' '
	if policy.Extensions["example"][0] != '{' {
		t.Fatal("clone aliases policy settings")
	}
	raw, err := json.Marshal(accessPolicyBody(policy))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := parseAccessPolicy("demo", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !actual.RestrictsAccess() || actual.AllowsDownload() || string(actual.Extensions["example"]) != string(policy.Extensions["example"]) {
		t.Fatal(string(raw))
	}
	for _, invalid := range []string{
		`{"extensions":null}`, `{"extensions":[]}`, `{"extensions":{"example":null}}`,
		`{"extensions":{"example":true}}`, `{"extensions":{"bad/name":{}}}`,
		`{"extensions":{},"EXTENSIONS":{}}`,
	} {
		if _, err := parseAccessPolicy("demo", []byte(invalid)); err == nil {
			t.Fatalf("accepted invalid policy %s", invalid)
		}
	}
}

func TestExtensionSettingsRequireInternalViewerAuthority(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"],"extensions":{"example":{"theme":"blue"},"other":{"private":"other-settings"}}}`)
	path := "/api/sites/private/extensions/example"
	if rec := st.do(siteRequest(http.MethodGet, "sites.localhost:8443", path)); rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous extension read = %d", rec.Code)
	}
	if rec := st.do(asForwardUser(siteRequest(http.MethodGet, "sites.localhost:8443", path), "viewer@example.com")); rec.Code != http.StatusForbidden {
		t.Fatalf("non-viewer extension read = %d", rec.Code)
	}
	rec := st.do(asForwardUser(siteRequest(http.MethodGet, "sites.localhost:8443", path), "owner@example.com"))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner extension read = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"theme":"blue"`) || strings.Contains(rec.Body.String(), "other-settings") || strings.Contains(rec.Body.String(), "owner@example.com") {
		t.Fatal("extension read leaked policy or another namespace: " + rec.Body.String())
	}
}
