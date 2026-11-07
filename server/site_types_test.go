package main

import (
	"archive/zip"
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestParseTypePackageAcceptsTheExample(t *testing.T) {
	files := examplePackage(t)
	var deploy []deployFile
	for p, data := range files {
		deploy = append(deploy, deployFile{path: p, data: []byte(data)})
	}
	pkg, terr := parseTypePackage(deploy)
	if terr != nil {
		t.Fatalf("example package rejected: %v", terr)
	}
	if pkg.Name != "card" || pkg.Version != "1.0.0" || pkg.Title != "Card" || !pkg.HasSkill {
		t.Fatalf("parsed package = %+v", pkg)
	}
	if !strings.HasPrefix(pkg.Hash, "sha256:") || len(pkg.Files) != len(files) {
		t.Fatalf("hash %q, %d inventory files", pkg.Hash, len(pkg.Files))
	}
	for i := 1; i < len(pkg.Files); i++ {
		if pkg.Files[i-1].Path >= pkg.Files[i].Path {
			t.Fatalf("inventory is not sorted: %v", pkg.Files)
		}
	}
	// The identity is independent of upload order.
	reversed := make([]deployFile, len(deploy))
	for i := range deploy {
		reversed[len(deploy)-1-i] = deploy[i]
	}
	again, _ := parseTypePackage(reversed)
	if again.Hash != pkg.Hash {
		t.Fatalf("hash depends on order: %s vs %s", again.Hash, pkg.Hash)
	}
}

func TestParseTypePackageRejections(t *testing.T) {
	manifest := func(extra string) string {
		return `{"format":1,"name":"card","version":"1.0.0","title":"Card","data":["card.json"]` + extra + `}`
	}
	base := func() map[string]string {
		return map[string]string{typeManifestFile: manifest(""), typeEntryFile: "<h1>card</h1>"}
	}
	for _, tt := range []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"no manifest", func(f map[string]string) { delete(f, typeManifestFile) }, "type.json"},
		{"no entry", func(f map[string]string) { delete(f, typeEntryFile) }, "index.html"},
		{"dot file", func(f map[string]string) { f[".DS_Store"] = "x" }, ".DS_Store"},
		{"nested dot dir", func(f map[string]string) { f["lib/.git/config"] = "x" }, ".git"},
		{"node_modules", func(f map[string]string) { f["node_modules/x.js"] = "x" }, "node_modules"},
		{"absolute path", func(f map[string]string) { f["/app.js"] = "x" }, "relative"},
		{"traversal", func(f map[string]string) { f["a/../b.js"] = "x" }, ".."},
		{"backslash", func(f map[string]string) { f[`a\b.js`] = "x" }, "forward slashes"},
		{"file and dir collide", func(f map[string]string) { f["lib"] = "x"; f["lib/a.js"] = "y" }, "conflicts"},
		{"unknown manifest field", func(f map[string]string) { f[typeManifestFile] = manifest(`,"entry":"x"`) }, "entry"},
		{"bad format", func(f map[string]string) {
			f[typeManifestFile] = strings.Replace(manifest(""), `"format":1`, `"format":2`, 1)
		}, "format"},
		{"bad name", func(f map[string]string) { f[typeManifestFile] = strings.Replace(manifest(""), `"card"`, `"Card_"`, 1) }, "name"},
		{"bad version", func(f map[string]string) { f[typeManifestFile] = strings.Replace(manifest(""), `"1.0.0"`, `"1.0"`, 1) }, "MAJOR"},
		{"empty title", func(f map[string]string) { f[typeManifestFile] = strings.Replace(manifest(""), `"Card"`, `" "`, 1) }, "title"},
		{"no data", func(f map[string]string) {
			f[typeManifestFile] = strings.Replace(manifest(""), `["card.json"]`, `[]`, 1)
		}, "data"},
		{"glob data", func(f map[string]string) {
			f[typeManifestFile] = strings.Replace(manifest(""), `"card.json"`, `"*.json"`, 1)
		}, "glob"},
		{"duplicate data", func(f map[string]string) {
			f[typeManifestFile] = strings.Replace(manifest(""), `["card.json"]`, `["card.json","card.json"]`, 1)
		}, "twice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := base()
			tt.mutate(files)
			var deploy []deployFile
			for p, data := range files {
				deploy = append(deploy, deployFile{path: p, data: []byte(data)})
			}
			_, terr := parseTypePackage(deploy)
			if terr == nil || terr.code != "invalid_package" || !strings.Contains(terr.message, tt.want) {
				t.Fatalf("parseTypePackage = %v, want invalid_package mentioning %q", terr, tt.want)
			}
		})
	}
}

func TestParseTypePackageRejectsReservedDataPaths(t *testing.T) {
	for _, entry := range []string{"index.html", "_type/", "_type/x.json", "_access.json", "_spot.json",
		"_screenshot.png", "spot.js", "spot-live.js", "spot.d.ts", "api/", "api/data.json"} {
		files := []deployFile{
			{path: typeManifestFile, data: []byte(`{"format":1,"name":"card","version":"1.0.0","title":"Card","data":["` + entry + `"]}`)},
			{path: typeEntryFile, data: []byte("x")},
		}
		if _, terr := parseTypePackage(files); terr == nil || !strings.Contains(terr.message, "reserves") {
			t.Errorf("data entry %q accepted, want reserved-name rejection (got %v)", entry, terr)
		}
	}
}

func TestTypeUploadRequiresPlatformAdmin(t *testing.T) {
	env := newTypesEnv(t)
	env.as(typesOwnerEmail)
	if rec := env.do(typeUploadRequest(t, examplePackage(t))); rec.Code != http.StatusForbidden || errorCode(t, rec) != "admin_required" {
		t.Fatalf("non-admin upload = %d %s, want 403 admin_required", rec.Code, rec.Body.String())
	}
	env.as(typesAdminEmail)
	req := typeUploadRequest(t, examplePackage(t))
	req.Header.Set("Authorization", "Bearer spot_pk_whatever")
	if rec := env.do(req); rec.Code != http.StatusForbidden || errorCode(t, rec) != "admin_required" {
		t.Fatalf("publishing-key upload = %d %s, want 403 admin_required", rec.Code, rec.Body.String())
	}
	site := typeUploadRequest(t, examplePackage(t))
	site.Host = "demo.spot.localhost"
	if rec := env.do(site); rec.Code != http.StatusBadRequest {
		t.Fatalf("upload on a site host = %d, want 400", rec.Code)
	}
}

func TestTypeUploadIsIdempotentAndImmutable(t *testing.T) {
	env := newTypesEnv(t)
	first := env.upload(examplePackage(t))
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload = %d %s", first.Code, first.Body.String())
	}
	body := decodeJSON[map[string]any](t, first)
	if body["created"] != true || body["type"].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("first upload body = %v", body)
	}
	hash := body["type"].(map[string]any)["hash"]

	// Yank, then re-upload the same bytes: still 200, still yanked.
	env.as(typesAdminEmail)
	if rec := env.jsonRequest(http.MethodPost, "/api/types/card/1.0.0/yank", ""); rec.Code != http.StatusOK {
		t.Fatalf("yank = %d %s", rec.Code, rec.Body.String())
	}
	again := env.upload(examplePackage(t))
	if again.Code != http.StatusOK || decodeJSON[map[string]any](t, again)["created"] != false {
		t.Fatalf("identical re-upload = %d %s, want 200 created=false", again.Code, again.Body.String())
	}
	if v, _ := env.srv.types.Version(t.Context(), "card", "1.0.0"); !v.Yanked() {
		t.Fatal("identical re-upload cleared the yank")
	}

	changed := examplePackage(t)
	changed["card.js"] += "\n// changed\n"
	rec := env.upload(changed)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "type_version_exists" {
		t.Fatalf("changed bytes under 1.0.0 = %d %s, want 409 type_version_exists", rec.Code, rec.Body.String())
	}
	if decodeJSON[map[string]any](t, rec)["hash"] != hash {
		t.Fatalf("conflict did not report the recorded hash: %s", rec.Body.String())
	}
	// Build metadata is not identity.
	build := examplePackage(t)
	build[typeManifestFile] = strings.Replace(build[typeManifestFile], `"1.0.0"`, `"1.0.0+ci.7"`, 1)
	if rec := env.upload(build); rec.Code != http.StatusConflict {
		t.Fatalf("1.0.0+ci.7 with a different manifest = %d, want 409 against 1.0.0", rec.Code)
	}
}

// Concurrent uploads of different bytes under one version: exactly one
// wins, and the stored bytes are the winner's.
func TestConcurrentConflictingUploadsKeepOneVersion(t *testing.T) {
	env := newTypesEnv(t)
	env.as(typesAdminEmail)
	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		files := examplePackage(t)
		files["card.js"] += strings.Repeat("\n// variant", i+1)
		req := typeUploadRequest(t, files)
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = env.do(req).Code
		}()
	}
	wg.Wait()
	created := 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("upload status %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("created %d versions, want exactly 1 (%v)", created, codes)
	}
	v, err := env.srv.types.Version(t.Context(), "card", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	file, _ := v.file("card.js")
	rc, err := env.srv.sites.(TypeFileStorage).OpenTypeFile(t.Context(), "card", "1.0.0", "card.js")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var stored bytes.Buffer
	stored.ReadFrom(rc)
	if got := sha256Hex(stored.Bytes()); got != file.SHA256 {
		t.Fatalf("stored card.js hash %s, recorded %s", got, file.SHA256)
	}
}

func TestTypeReadAPI(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustUpload(cardVersion(t, "1.1.0"))
	env.mustUpload(cardVersion(t, "1.2.0-rc.1"))
	noSkill := cardVersion(t, "1.0.1")
	delete(noSkill, typeSkillFile)
	env.mustUpload(noSkill)

	env.as(typesOwnerEmail)
	list := decodeJSON[map[string][]map[string]any](t, env.get("spot.localhost", "/api/types"))["types"]
	if len(list) != 1 || list[0]["name"] != "card" || list[0]["version"] != "1.1.0" || list[0]["skill"] != true {
		t.Fatalf("types list = %v, want card@1.1.0", list)
	}
	if _, ok := list[0]["selectable"]; ok {
		t.Fatalf("default listing carries selectable: %v", list[0])
	}

	versions := decodeJSON[map[string]any](t, env.get("spot.localhost", "/api/types/card"))
	if versions["latest"] != "1.1.0" {
		t.Fatalf("latest = %v", versions["latest"])
	}
	var order []string
	for _, v := range versions["versions"].([]any) {
		entry := v.(map[string]any)
		order = append(order, entry["version"].(string))
		if _, ok := entry["pins"]; ok {
			t.Fatalf("non-admin sees pins: %v", entry)
		}
	}
	if strings.Join(order, ",") != "1.2.0-rc.1,1.1.0,1.0.1,1.0.0" {
		t.Fatalf("version order = %v", order)
	}
	env.as(typesAdminEmail)
	adminVersions := decodeJSON[map[string]any](t, env.get("spot.localhost", "/api/types/card"))
	first := adminVersions["versions"].([]any)[0].(map[string]any)
	if first["pins"] != float64(0) || first["uploaded_by"] != typesAdminEmail {
		t.Fatalf("admin version entry = %v", first)
	}

	env.as(typesOwnerEmail)
	detail := decodeJSON[map[string]any](t, env.get("spot.localhost", "/api/types/card/1.1.0"))
	if detail["hash"] == "" || len(detail["files"].([]any)) != 5 || detail["manifest"].(map[string]any)["version"] != "1.1.0" {
		t.Fatalf("detail = %v", detail)
	}
	if data := detail["data"].([]any); len(data) != 2 || data[0] != "card.json" || data[1] != "media/" {
		t.Fatalf("detail data = %v", data)
	}

	skill := env.get("spot.localhost", "/api/types/card/1.1.0/skill")
	if skill.Code != http.StatusOK || skill.Header().Get("Content-Type") != "text/markdown; charset=utf-8" ||
		!strings.Contains(skill.Body.String(), "# Card") {
		t.Fatalf("skill = %d %s %q", skill.Code, skill.Header().Get("Content-Type"), skill.Body.String())
	}
	if rec := env.get("spot.localhost", "/api/types/card/1.0.1/skill"); rec.Code != http.StatusNotFound || errorCode(t, rec) != "type_skill_not_found" {
		t.Fatalf("missing skill = %d %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{"/api/types/nope", "/api/types/card/9.9.9", "/api/types/card/1.1.0+x", "/api/types/card/v1"} {
		if rec := env.get("spot.localhost", target); rec.Code != http.StatusNotFound || errorCode(t, rec) != "type_not_found" {
			t.Errorf("GET %s = %d %s, want 404 type_not_found", target, rec.Code, rec.Body.String())
		}
	}

	pkg := env.get("spot.localhost", "/api/types/card/1.1.0/package")
	if pkg.Code != http.StatusOK || pkg.Header().Get("Content-Type") != "application/zip" ||
		!strings.Contains(pkg.Header().Get("Content-Disposition"), "card-1.1.0.zip") {
		t.Fatalf("package = %d %v", pkg.Code, pkg.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(pkg.Body.Bytes()), int64(pkg.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "index.html,_type/card.css,_type/card.js,_type/index.html" {
		t.Fatalf("package entries = %v", names)
	}

	// Identified callers only, and only on the apex.
	env.srv.resolver = nil
	if rec := env.get("spot.localhost", "/api/types"); rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d, want refusal", rec.Code)
	}
	env.as(typesOwnerEmail)
	if rec := env.get("demo.spot.localhost", "/api/types"); rec.Code != http.StatusBadRequest {
		t.Fatalf("list on a site host = %d, want 400", rec.Code)
	}
}

func TestTypeListAllIncludesUnselectableTypes(t *testing.T) {
	env := newTypesEnv(t)
	for _, files := range []map[string]string{
		cardVersion(t, "1.0.0"), cardVersion(t, "1.1.0"), cardVersion(t, "2.0.0-rc.1"),
		renamedType(t, "draft", "0.1.0-rc.1"), renamedType(t, "draft", "0.1.0-rc.2"),
		renamedType(t, "gone", "1.0.0"), renamedType(t, "gone", "1.1.0"),
		renamedType(t, "mixed", "1.0.0"), renamedType(t, "mixed", "1.1.0"),
	} {
		env.mustUpload(files)
	}
	env.as(typesAdminEmail)
	for _, target := range []string{"/api/types/gone/1.0.0/yank", "/api/types/gone/1.1.0/yank", "/api/types/mixed/1.1.0/yank"} {
		if rec := env.jsonRequest(http.MethodPost, target, ""); rec.Code != http.StatusOK {
			t.Fatalf("yank %s = %d", target, rec.Code)
		}
	}
	env.as(typesOwnerEmail)

	summarize := func(rec map[string][]map[string]any) string {
		var out []string
		for _, entry := range rec["types"] {
			s := entry["name"].(string) + "@" + entry["version"].(string)
			if selectable, ok := entry["selectable"]; ok {
				if selectable == true {
					s += "+"
				} else {
					s += "-"
				}
			}
			out = append(out, s)
		}
		return strings.Join(out, " ")
	}
	if got := summarize(decodeJSON[map[string][]map[string]any](t, env.get("spot.localhost", "/api/types"))); got != "card@1.1.0 mixed@1.0.0" {
		t.Fatalf("default list = %q", got)
	}
	if got := summarize(decodeJSON[map[string][]map[string]any](t, env.get("spot.localhost", "/api/types?all=false"))); got != "card@1.1.0 mixed@1.0.0" {
		t.Fatalf("all=false list = %q", got)
	}
	want := "card@1.1.0+ draft@0.1.0-rc.2- gone@1.1.0- mixed@1.0.0+"
	if got := summarize(decodeJSON[map[string][]map[string]any](t, env.get("spot.localhost", "/api/types?all=true"))); got != want {
		t.Fatalf("all=true list = %q, want %q", got, want)
	}
	if rec := env.get("spot.localhost", "/api/types?all=yes"); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_type_field" {
		t.Fatalf("all=yes = %d %s", rec.Code, rec.Body.String())
	}
}

func renamedType(t *testing.T, name, version string) map[string]string {
	t.Helper()
	files := cardVersion(t, version)
	files[typeManifestFile] = strings.Replace(files[typeManifestFile], `"name":"card"`, `"name":"`+name+`"`, 1)
	return files
}

func TestTypeYankRequiresAdminAndIsIdempotent(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(examplePackage(t))
	env.as(typesOwnerEmail)
	if rec := env.jsonRequest(http.MethodPost, "/api/types/card/1.0.0/yank", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("owner yank = %d, want 403", rec.Code)
	}
	env.as(typesAdminEmail)
	first := decodeJSON[map[string]any](t, env.jsonRequest(http.MethodPost, "/api/types/card/1.0.0/yank", ""))
	second := decodeJSON[map[string]any](t, env.jsonRequest(http.MethodPost, "/api/types/card/1.0.0/yank", ""))
	if first["yanked"] != true || first["yanked_at"] != second["yanked_at"] {
		t.Fatalf("repeated yank changed yanked_at: %v vs %v", first, second)
	}
	unyank := decodeJSON[map[string]any](t, env.jsonRequest(http.MethodDelete, "/api/types/card/1.0.0/yank", ""))
	if unyank["yanked"] != false || unyank["yanked_at"] != nil {
		t.Fatalf("un-yank = %v", unyank)
	}
	if rec := env.jsonRequest(http.MethodPost, "/api/types/card/2.0.0/yank", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("yank unknown = %d, want 404", rec.Code)
	}
}
