package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

func nginxSite(name, root string) string {
	return fmt.Sprintf("server {\n\tlisten 80;\n\tserver_name %s;\n\troot %s;\n}\n", name, root)
}

func jsonFile(t *testing.T, value any) *fstest.MapFile {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return mapFile(data)
}

// applicationsHost serves a classic WordPress, a Bedrock WordPress, a
// Laravel deploy behind current/ (twice) and a static site.
func applicationsHost(t *testing.T) (*Host, *readTracker) {
	t.Helper()
	classic := "var/www/vhosts/blog.test/httpdocs"
	bedrock := "var/www/vhosts/bedrock.test/site"
	shop := "home/forge/shop.test"
	sites := nginxSite("blog.test", "/"+classic) +
		nginxSite("bedrock.test", "/"+bedrock+"/web") +
		nginxSite("shop.test", "/"+shop+"/current/public") +
		nginxSite("admin.shop.test", "/"+shop+"/current/public") +
		nginxSite("static.test", "/var/www/vhosts/static.test/httpdocs")
	fsys := fstest.MapFS{
		"usr/sbin/nginx":       mapFile(nil),
		"etc/nginx/nginx.conf": mapFile([]byte("events {}\nhttp {\n" + sites + "}\n")),
		"etc/passwd":           mapFile([]byte("root:x:0:0:root:/root:/bin/bash\nforge:x:1000:1000::/home/forge:/bin/bash\n")),

		classic + "/index.php":                                            mapFile([]byte("<?php require 'wp-blog-header.php';")),
		classic + "/wp-config.php":                                        mapFile([]byte("<?php define('DB_PASSWORD', 'secret');")),
		classic + "/.env":                                                 mapFile([]byte("SECRET=1")),
		classic + "/wp-includes/version.php":                              mapFile([]byte("<?php\n/**\n * The WordPress version string.\n */\n$wp_version = '6.6.2';\n$wp_db_version = 57155;\n")),
		classic + "/wp-content/plugins/index.php":                         mapFile([]byte("<?php // Silence is golden.")),
		classic + "/wp-content/plugins/hello.php":                         mapFile([]byte("<?php\n/*\nPlugin Name: Hello Dolly\nVersion: 1.7.2\n*/\n")),
		classic + "/wp-content/plugins/woocommerce/includes/class-wc.php": mapFile([]byte("<?php\n/**\n * Plugin Name: Not the main file\n */")),
		classic + "/wp-content/plugins/woocommerce/uninstall.php":         mapFile([]byte("<?php // uninstall")),
		classic + "/wp-content/plugins/woocommerce/woocommerce.php":       mapFile([]byte("<?php\r\n/**\r\n * Plugin Name: WooCommerce\r\n * Version: 9.3.1 */\r\n")),
		classic + "/wp-content/plugins/woocommerce/composer.lock":         jsonFile(t, map[string]any{"packages": []any{map[string]any{"name": "nested/never-read", "version": "1.0.0"}}}),
		classic + "/wp-content/plugins/empty/readme.txt":                  mapFile(nil),
		classic + "/wp-content/mu-plugins/index.php":                      mapFile(nil),
		classic + "/wp-content/mu-plugins/loader.php":                     mapFile([]byte("<?php require __DIR__ . '/x/x.php';")),
		classic + "/wp-content/themes/twentytwentyfour/style.css":         mapFile([]byte("/*\nTheme Name: Twenty Twenty-Four\nVersion: 1.2\n*/")),
		classic + "/wp-content/themes/child/style.css":                    mapFile([]byte("/*\nTheme Name: Child\nTemplate: twentytwentyfour\n*/")),
		classic + "/wp-content/themes/bundle/agency/style.css":            mapFile([]byte("/*\nTheme Name: Agency\nVersion: 2.0.1\n*/")),
		classic + "/backup/composer.lock":                                 jsonFile(t, map[string]any{"packages": []any{map[string]any{"name": "backup/never-read", "version": "1.0.0"}}}),

		bedrock + "/composer.json": jsonFile(t, map[string]any{"require": map[string]string{"roots/wordpress": "^6.6", "wpackagist-plugin/akismet": "^5.3"}}),
		bedrock + "/composer.lock": jsonFile(t, map[string]any{"packages": []any{
			map[string]any{"name": "roots/wordpress", "version": "6.6.1"},
			map[string]any{"name": "wpackagist-plugin/akismet", "version": "5.3.3"},
			map[string]any{"name": "vlucas/phpdotenv", "version": "v5.6.1"},
		}}),
		bedrock + "/web/wp-config.php":                   mapFile(nil),
		bedrock + "/web/wp/wp-includes/version.php":      mapFile([]byte("<?php\n$wp_version = '6.6.1';\n")),
		bedrock + "/web/app/plugins/akismet/akismet.php": mapFile([]byte("<?php\n/*\nPlugin Name: Akismet Anti-spam\nVersion: 5.3.3\n*/")),

		shop + "/current":                     symlink("releases/2"),
		shop + "/releases/1/composer.lock":    jsonFile(t, map[string]any{"packages": []any{map[string]any{"name": "old/never-read", "version": "1.0.0"}}}),
		shop + "/releases/2/artisan":          mapFile(nil),
		shop + "/releases/2/.env":             mapFile([]byte("APP_KEY=base64:secret")),
		shop + "/releases/2/public/index.php": mapFile(nil),
		shop + "/releases/2/composer.json": jsonFile(t, map[string]any{
			"require":     map[string]string{"php": "^8.3", "laravel/framework": "^11.9"},
			"require-dev": map[string]string{"phpunit/phpunit": "^11.0"},
		}),
		shop + "/releases/2/composer.lock": jsonFile(t, map[string]any{
			"packages":     []any{map[string]any{"name": "laravel/framework", "version": "v11.9.2"}, map[string]any{"name": "guzzlehttp/guzzle", "version": "7.9.2"}},
			"packages-dev": []any{map[string]any{"name": "phpunit/phpunit", "version": "11.3.1"}},
		}),
		shop + "/releases/2/package.json": jsonFile(t, map[string]any{"dependencies": map[string]string{"axios": "^1.7"}, "devDependencies": map[string]string{"vite": "^5.4"}}),
		shop + "/releases/2/package-lock.json": jsonFile(t, map[string]any{"lockfileVersion": 3, "packages": map[string]any{
			"":                                       map[string]any{"name": "shop"},
			"node_modules/axios":                     map[string]any{"version": "1.7.7"},
			"node_modules/follow-redirects":          map[string]any{"version": "1.15.9"},
			"node_modules/vite":                      map[string]any{"version": "5.4.8", "dev": true},
			"node_modules/vite/node_modules/esbuild": map[string]any{"version": "0.21.5", "dev": true},
			"node_modules/local":                     map[string]any{"resolved": "../local", "link": true},
		}}),
		shop + "/releases/2/vendor/laravel/framework/composer.lock": mapFile([]byte("{}")),

		"var/www/vhosts/static.test/httpdocs/index.html": mapFile(nil),
	}
	tracker := &readTracker{MapFS: fsys}
	return &Host{FS: tracker, Root: "/", Now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }}, tracker
}

func collectApplications(t *testing.T, collector *Applications) *ApplicationsInfo {
	t.Helper()
	payload, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return payload.(*ApplicationsInfo)
}

func ptr(value string) *string { return &value }

func TestApplications(t *testing.T) {
	host, tracker := applicationsHost(t)
	info := collectApplications(t, NewApplications(host, nil))

	var paths []string
	for _, item := range info.Items {
		paths = append(paths, item.Path)
	}
	want := []string{"/home/forge/shop.test/current", "/var/www/vhosts/bedrock.test/site", "/var/www/vhosts/blog.test/httpdocs"}
	if !slices.Equal(paths, want) || info.Truncated {
		t.Fatalf("paths = %v, truncated = %v", paths, info.Truncated)
	}

	shop := info.Items[0]
	if shop.Kind != ApplicationKindLaravel || *shop.Version != "11.9.2" || !slices.Equal(shop.Sites, []string{"nginx/admin.shop.test", "nginx/shop.test"}) || shop.Owner == nil || *shop.Owner != "forge" {
		t.Errorf("shop = %+v", shop)
	}
	if !slices.Equal(shop.Lockfiles, []string{"composer.lock", "package-lock.json"}) {
		t.Errorf("shop lockfiles = %v", shop.Lockfiles)
	}
	wantPackages := []ApplicationPackage{
		{EcosystemComposer, "guzzlehttp/guzzle", "7.9.2", false, false},
		{EcosystemComposer, "laravel/framework", "v11.9.2", true, false},
		{EcosystemComposer, "phpunit/phpunit", "11.3.1", true, true},
		{EcosystemNPM, "axios", "1.7.7", true, false},
		{EcosystemNPM, "esbuild", "0.21.5", false, true},
		{EcosystemNPM, "follow-redirects", "1.15.9", false, false},
		{EcosystemNPM, "vite", "5.4.8", true, true},
	}
	if !slices.Equal(shop.Packages, wantPackages) {
		t.Errorf("shop packages\n got %+v\nwant %+v", shop.Packages, wantPackages)
	}

	bedrock := info.Items[1]
	if bedrock.Kind != ApplicationKindWordPress || *bedrock.Version != "6.6.1" || len(bedrock.Packages) != 3 || !bedrock.Packages[2].Direct {
		t.Errorf("bedrock = %+v", bedrock)
	}
	if got := bedrock.WordPress.Plugins; len(got) != 1 || got[0].Slug != "akismet" || *got[0].Version != "5.3.3" {
		t.Errorf("bedrock plugins = %+v", got)
	}

	blog := info.Items[2]
	if blog.Kind != ApplicationKindWordPress || *blog.Version != "6.6.2" || len(blog.Packages) != 0 || len(blog.Lockfiles) != 0 {
		t.Errorf("blog = %+v", blog)
	}
	wantPlugins := []WordPressPlugin{
		{Slug: "hello", Name: ptr("Hello Dolly"), Version: ptr("1.7.2")},
		{Slug: "woocommerce", Name: ptr("WooCommerce"), Version: ptr("9.3.1")},
		{Slug: "loader", MustUse: true},
	}
	if got, _ := json.Marshal(blog.WordPress.Plugins); string(got) != mustJSON(t, wantPlugins) {
		t.Errorf("blog plugins = %s", got)
	}
	wantThemes := []WordPressTheme{
		{Slug: "bundle/agency", Name: ptr("Agency"), Version: ptr("2.0.1")},
		{Slug: "child", Name: ptr("Child"), Parent: ptr("twentytwentyfour")},
		{Slug: "twentytwentyfour", Name: ptr("Twenty Twenty-Four"), Version: ptr("1.2")},
	}
	if got, _ := json.Marshal(blog.WordPress.Themes); string(got) != mustJSON(t, wantThemes) {
		t.Errorf("blog themes = %s", got)
	}

	// Configuration files and nested, backed up or old lockfiles are never opened.
	for name := range tracker.opens {
		if strings.HasSuffix(name, "wp-config.php") || strings.HasSuffix(name, ".env") || strings.Contains(name, "never") ||
			strings.Contains(name, "releases/1/") || strings.Contains(name, "/vendor/") || strings.Contains(name, "/backup/") || strings.Contains(name, "woocommerce/composer.lock") {
			t.Errorf("opened %s", name)
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplicationsDeltaKeyAndHashRoundTrip(t *testing.T) {
	host, _ := applicationsHost(t)
	collector := NewApplications(host, nil)
	first, err := inventory.NewDataset("applications", collectApplications(t, collector))
	if err != nil {
		t.Fatal(err)
	}
	second, err := inventory.NewDataset("applications", collectApplications(t, collector))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash {
		t.Fatal("hash changed between identical collections")
	}
	if !slices.Equal(collector.DeltaKeyFields(), []string{"path"}) {
		t.Fatalf("key fields = %v", collector.DeltaKeyFields())
	}
}

func TestApplicationsChangeToken(t *testing.T) {
	host, tracker := applicationsHost(t)
	collector := NewApplications(host, nil)
	collectApplications(t, collector)
	before := collector.ChangeToken()
	if collector.ChangeToken() != before {
		t.Fatal("token is not stable")
	}
	changed := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{
		"home/forge/shop.test/releases/2/composer.lock",
		"var/www/vhosts/blog.test/httpdocs/wp-includes/version.php",
		"var/www/vhosts/blog.test/httpdocs/wp-content/plugins/hello.php",
	} {
		tracker.MapFS[name].ModTime = changed
		after := collector.ChangeToken()
		if after == before {
			t.Fatalf("changing %s kept the token", name)
		}
		before = after
	}
	tracker.MapFS["var/www/vhosts/blog.test/httpdocs/wp-content/plugins/akismet/akismet.php"] = mapFile(nil)
	if collector.ChangeToken() == before {
		t.Fatal("a new plugin kept the token")
	}
}

func TestApplicationRootStaysInTheUsersTree(t *testing.T) {
	host := &Host{FS: fstest.MapFS{
		"home/forge/package.json":           mapFile([]byte("{}")),
		"home/forge/static/public/a.html":   mapFile(nil),
		"var/www/html/index.html":           mapFile(nil),
		"var/package.json":                  mapFile([]byte("{}")),
		"opt/app/composer.json":             mapFile([]byte("{}")),
		"opt/app/public/index.php":          mapFile(nil),
		"home/forge/site/composer.json":     mapFile([]byte("{}")),
		"home/forge/site/a/b/c/d/index.php": mapFile(nil),
	}}
	run := &applicationRun{host: host}
	for documentRoot, want := range map[string]string{
		// A home directory's stray manifest still marks it, but nothing
		// above the hosting user's tree or at the top level does.
		"/home/forge/static/public": "/home/forge",
		"/var/www/html":             "",
		"/opt/app/public":           "/opt/app",
		"/home/forge/site/a/b/c/d":  "",
		"relative/path":             "",
	} {
		if got := run.applicationRoot(documentRoot); got != want {
			t.Errorf("applicationRoot(%s) = %q, want %q", documentRoot, got, want)
		}
	}
}

func TestApplicationsLimits(t *testing.T) {
	host, tracker := applicationsHost(t)
	var packages []any
	for index := range maxLockfilePackages + 10 {
		packages = append(packages, map[string]any{"name": fmt.Sprintf("vendor/package-%d", index), "version": "1.0.0"})
	}
	tracker.MapFS["home/forge/shop.test/releases/2/composer.lock"] = jsonFile(t, map[string]any{"packages": packages})
	info := collectApplications(t, NewApplications(host, nil))
	shop := info.Items[0]
	if !shop.Truncated || len(shop.Packages) != maxLockfilePackages+4 {
		t.Fatalf("truncated = %v, packages = %d", shop.Truncated, len(shop.Packages))
	}
	if info.Truncated || info.Items[1].Truncated {
		t.Fatal("other applications are truncated")
	}
}

func TestHeaderField(t *testing.T) {
	for text, want := range map[string]*string{
		"<?php\n/**\n * Plugin Name: Classic Editor\n */":     ptr("Classic Editor"),
		"<?php /* Plugin Name: Inline */ ?>":                  ptr("Inline"),
		"<?php\n# plugin name:   Lower Case  \n":              ptr("Lower Case"),
		"<?php\n * @wordpress-plugin\n * Plugin Name: Tagged": ptr("Tagged"),
		"<?php\n * Plugin Name:\n":                            nil,
		"<?php\n$x = 'Plugin Name: In code';":                 nil,
	} {
		got := headerField(text, "Plugin Name")
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Errorf("headerField(%q) = %v, want %v", text, got, want)
		}
	}
}

func packagesString(packages []ApplicationPackage) string {
	slices.SortFunc(packages, func(a, b ApplicationPackage) int { return cmpStrings(a.Name, b.Name, a.Version, b.Version) })
	var parts []string
	for _, entry := range packages {
		flags := ""
		if entry.Direct {
			flags += "+direct"
		}
		if entry.Dev {
			flags += "+dev"
		}
		parts = append(parts, entry.Name+"@"+entry.Version+flags)
	}
	return strings.Join(parts, " ")
}

func npmManifest(production, dev map[string]string) *manifestDependencies {
	return &manifestDependencies{production: production, dev: dev}
}

func TestParseYarnLock(t *testing.T) {
	manifest := npmManifest(map[string]string{"axios": "^1.7.0", "aliased": "npm:lodash@^4.17.0"}, map[string]string{"vite": "^5.4.0"})
	classic := `# THIS IS AN AUTOGENERATED FILE. DO NOT EDIT THIS FILE DIRECTLY.
# yarn lockfile v1


aliased@npm:lodash@^4.17.0:
  version "4.17.21"

axios@^1.7.0:
  version "1.7.7"
  resolved "https://registry.yarnpkg.com/axios/-/axios-1.7.7.tgz"
  dependencies:
    follow-redirects "^1.15.6"

"follow-redirects@^1.15.0", follow-redirects@^1.15.6:
  version "1.15.9"

vite@^5.4.0:
  version "5.4.8"
  dependencies:
    esbuild "^0.21.3"
  optionalDependencies:
    fsevents "~2.3.3"

esbuild@^0.21.3:
  version "0.21.5"

fsevents@~2.3.3:
  version "2.3.3"
`
	packages, err := parseYarnLock([]byte(classic), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := packagesString(packages), "axios@1.7.7+direct esbuild@0.21.5+dev follow-redirects@1.15.9 fsevents@2.3.3+dev lodash@4.17.21 vite@5.4.8+direct+dev"; got != want {
		t.Errorf("yarn v1\n got %s\nwant %s", got, want)
	}

	berry := `# This file is generated by running "yarn install" inside your project.

__metadata:
  version: 8
  cacheKey: 10c0

"axios@npm:^1.7.0":
  version: 1.7.7
  resolution: "axios@npm:1.7.7"
  dependencies:
    follow-redirects: "npm:^1.15.6"
  languageName: node
  linkType: hard

"follow-redirects@npm:^1.15.6":
  version: 1.15.9
  resolution: "follow-redirects@npm:1.15.9"
  peerDependenciesMeta:
    debug:
      optional: true
  languageName: node
  linkType: hard

"shop@workspace:.":
  version: 0.0.0-use.local
  resolution: "shop@workspace:."
  dependencies:
    axios: "npm:^1.7.0"
  languageName: unknown
  linkType: soft

"vite@npm:^5.4.0":
  version: 5.4.8
  resolution: "vite@npm:5.4.8"
  languageName: node
  linkType: hard
`
	packages, err = parseYarnLock([]byte(berry), npmManifest(map[string]string{"axios": "^1.7.0"}, map[string]string{"vite": "^5.4.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := packagesString(packages), "axios@1.7.7+direct follow-redirects@1.15.9 vite@5.4.8+direct+dev"; got != want {
		t.Errorf("yarn berry\n got %s\nwant %s", got, want)
	}

	// Without a package.json nothing tells development dependencies apart.
	packages, _ = parseYarnLock([]byte(classic), nil)
	if got := packagesString(packages); strings.Contains(got, "+") {
		t.Errorf("without manifest = %s", got)
	}
}

func TestParsePnpmLock(t *testing.T) {
	version9 := `lockfileVersion: '9.0'

settings:
  autoInstallPeers: true

importers:

  .:
    dependencies:
      axios:
        specifier: ^1.7.0
        version: 1.7.7
    devDependencies:
      vite:
        specifier: ^5.4.0
        version: 5.4.8(@types/node@22.7.4)

packages:

  '@types/node@22.7.4':
    resolution: {integrity: sha512-x}

  axios@1.7.7:
    resolution: {integrity: sha512-x}

  follow-redirects@1.15.9:
    resolution: {integrity: sha512-x}

  vite@5.4.8:
    resolution: {integrity: sha512-x}

snapshots:

  '@types/node@22.7.4': {}

  axios@1.7.7:
    dependencies:
      follow-redirects: 1.15.9

  follow-redirects@1.15.9: {}

  vite@5.4.8(@types/node@22.7.4):
    dependencies:
      '@types/node': 22.7.4
`
	packages, err := parsePnpmLock([]byte(version9), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := packagesString(packages), "@types/node@22.7.4+dev axios@1.7.7+direct follow-redirects@1.15.9 vite@5.4.8+direct+dev"; got != want {
		t.Errorf("pnpm 9\n got %s\nwant %s", got, want)
	}

	version6 := `lockfileVersion: '6.0'

dependencies:
  axios:
    specifier: ^1.7.0
    version: 1.7.7

devDependencies:
  vite:
    specifier: ^5.4.0
    version: 5.4.8

packages:

  /axios@1.7.7:
    resolution: {integrity: sha512-x}
    dependencies:
      follow-redirects: 1.15.9
    dev: false

  /follow-redirects@1.15.9:
    resolution: {integrity: sha512-x}
    dev: false

  /vite@5.4.8:
    resolution: {integrity: sha512-x}
    dev: true
`
	packages, err = parsePnpmLock([]byte(version6), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := packagesString(packages), "axios@1.7.7+direct follow-redirects@1.15.9 vite@5.4.8+direct+dev"; got != want {
		t.Errorf("pnpm 6\n got %s\nwant %s", got, want)
	}
}

func TestParsePackageLockVersion1(t *testing.T) {
	lock := `{"lockfileVersion": 1, "dependencies": {
		"axios": {"version": "1.7.7", "dependencies": {"follow-redirects": {"version": "1.15.9"}}},
		"vite": {"version": "5.4.8", "dev": true},
		"local": {"version": "file:../local"}
	}}`
	packages, err := parsePackageLock([]byte(lock), npmManifest(map[string]string{"axios": "^1"}, map[string]string{"vite": "^5"}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := packagesString(packages), "axios@1.7.7+direct follow-redirects@1.15.9 vite@5.4.8+direct+dev"; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}
