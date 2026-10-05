package config

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestSharedWorkConfigIsExplicitAndValidated(t *testing.T) {
	legacy, err := Parse([]byte("[beads]\nprovider = \"file\"\n"))
	if err != nil || legacy.Beads.SharedWork != nil {
		t.Fatalf("legacy config selected shared mode: %+v, %v", legacy, err)
	}
	for _, body := range []string{
		"",
		"rig = \"team\"\n",
		"template = \"team/worker\"\n",
		"rig = \"team\"\ntemplate = \"team/worker\"\nlease = \"forever\"\n",
		"rig = \"team\"\ntemplate = \"team/worker\"\nlease = \"0s\"\n",
		"rig = \"team\"\ntemplate = \"team/worker\"\nmax_active = -1\n",
	} {
		if _, err := Parse([]byte("[beads.shared_work]\n" + body)); err == nil {
			t.Fatalf("invalid shared mode silently accepted: %q", body)
		}
	}
	selected, err := Parse([]byte("[beads.shared_work]\nrig = \"team\"\ntemplate = \"team/worker\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := selected.Beads.SharedWork.LeaseDuration()
	if err != nil || lease != 2*time.Minute || selected.Beads.SharedWork.ActiveLimit() != 1 {
		t.Fatalf("shared defaults: %+v, %v", selected.Beads.SharedWork, err)
	}
}

func TestSharedWorkConfigSurvivesUnrelatedBeadsFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["city/city.toml"] = []byte(`
include = ["fragment.toml"]
[workspace]
name = "shared"
[beads.shared_work]
rig = "team"
template = "team/worker"
lease = "3m"
`)
	fs.Files["city/fragment.toml"] = []byte("[beads]\nprovider = \"file\"\n")
	cfg, _, err := LoadWithIncludes(fs, "city/city.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Beads.SharedWork == nil || cfg.Beads.SharedWork.Lease != "3m" {
		t.Fatalf("shared mode silently downgraded by a fragment: %+v", cfg.Beads)
	}
}
