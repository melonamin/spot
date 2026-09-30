package main

import (
	"os"
	"strings"
	"testing"
)

func TestMaintainerUIAndSDKAssets(t *testing.T) {
	for _, path := range []string{"../sdk/index.html", "static_assets/sdk/index.html"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		for _, want := range []string{
			`id="access-maintainers"`,
			`['ai', 'slack', 'download']`,
			`policy.maintainers`,
			`People and groups who can deploy, delete, and publish this site`,
			`accessAllowPresent = false;`,
			`Mesh · no visitors`,
			`const normalized = key.toLowerCase();`,
			`throw new Error(key + ' must be an array of strings');`,
			`path !== '_access.json' || accessFileWarning`,
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s does not contain %q", path, want)
			}
		}
	}
	for _, path := range []string{"../sdk/spots.html", "static_assets/sdk/spots.html"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		for _, want := range []string{
			`/api/sites/manageable`,
			`armSiteDelete(site, row, release, 'Release name', 'Click again to release')`,
			`site.management_role === 'maintainer'`,
			`site.state === 'deleted'`,
			`Redeploy same name`,
			`Release name`,
			`site.management_role === 'admin'`,
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s does not contain %q", path, want)
			}
		}
	}
	for _, path := range []string{"../sdk/spot.js", "static_assets/sdk/spot.js"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `manageable: async`) {
			t.Fatalf("%s does not expose spot.sites.manageable()", path)
		}
	}
}

func TestMaintainerAgentGuidanceAssets(t *testing.T) {
	for _, path := range []string{"../sdk/agent.md", "static_assets/sdk/agent.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `maintainer delegation`) {
			t.Fatalf("%s does not describe maintainer guidance", path)
		}
	}
	for _, path := range []string{"../sdk/spot-agent-howto.md", "static_assets/sdk/spot-agent-howto.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		for _, want := range []string{
			`const manageable = await spot.sites.manageable();`,
			`"maintainers": ["bob@corp.com", "team-platform"]`,
			`A deploy cannot grant its own actor access`,
			`maintainer delete purges the site`,
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s does not contain %q", path, want)
			}
		}
	}
	for _, path := range []string{"../cli/spot", "../sdk/spot", "static_assets/sdk/spot"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		for _, want := range []string{
			`configuring visitor access or site maintainers`,
			`site access and maintainer delegation`,
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s does not contain %q", path, want)
			}
		}
	}
}
