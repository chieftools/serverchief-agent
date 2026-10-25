package collector

import (
	"reflect"
	"testing"
	"testing/fstest"
)

func phpConfig(prefix, version string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("#! /bin/sh\n\nSED=\"/usr/bin/sed\"\nprefix=\"" + prefix + "\"\ndatarootdir=\"/usr/local/php74/php\"\nexec_prefix=\"${prefix}\"\nversion=\"" + version + "\"\nvernum=\"70433\"\n")}
}

func TestReadPHPBuilds(t *testing.T) {
	host := &Host{FS: fstest.MapFS{
		// DirectAdmin: the default PHP in /usr/local and custombuilds per version
		"usr/local/bin/php-config":       phpConfig("/usr/local", "8.2.35"),
		"usr/local/php74/bin/php-config": phpConfig("/usr/local/php74", "7.4.33"),
		"usr/local/php82/bin/php-config": phpConfig("/usr/local/php82", "8.2.35"),
		// A source build with a release suffix in its directory
		"opt/php-8.2.35-2/bin/php-config": phpConfig("/opt/php-8.2.35-2", "8.2.35"),
		// Not a php-config the agent can read a version from
		"opt/php-broken/bin/php-config": &fstest.MapFile{Data: []byte("#!/bin/sh\nversion=$(cat VERSION)\n")},
		// Packages keep theirs elsewhere
		"usr/bin/php-config": phpConfig("/usr", "8.2.29"),
	}}

	want := []PHPBuild{
		{Prefix: "/opt/php-8.2.35-2", Version: "8.2.35"},
		{Prefix: "/usr/local", Version: "8.2.35"},
		{Prefix: "/usr/local/php74", Version: "7.4.33"},
		{Prefix: "/usr/local/php82", Version: "8.2.35"},
	}
	if got := host.readPHPBuilds(); !reflect.DeepEqual(got, want) {
		t.Errorf("builds = %+v, want %+v", got, want)
	}

	if got := (&Host{FS: fstest.MapFS{}}).readPHPBuilds(); got == nil || len(got) != 0 {
		t.Errorf("no builds = %#v, want an empty list", got)
	}
}
