package main

import (
	"slices"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/vfs"
)

func TestViewOptions(t *testing.T) {
	cmd := newMountCmd(&globalFlags{})
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	f := &mountFlags{views: vfs.DefaultViews, metaTTL: vfs.DefaultMetaTTL, dateZone: "Local"}
	o, err := f.viewOptions()
	if err != nil || !slices.Equal(o.Views, []string{"files", "tree", "by-date"}) || o.DateZone != time.Local {
		t.Errorf("defaults = %+v, %v", o, err)
	}
	if v, _ := cmd.Flags().GetStringSlice("views"); !slices.Equal(v, vfs.DefaultViews) {
		t.Errorf("--views default = %q", v)
	}
	f = &mountFlags{views: []string{"tags", "groups"}, metaTTL: time.Second, dateZone: "UTC"}
	if o, err := f.viewOptions(); err != nil || !slices.Equal(o.Views, []string{"tags", "groups"}) || o.DateZone != time.UTC {
		t.Errorf("tags,groups in UTC = %+v, %v", o, err)
	}
	for _, bad := range []mountFlags{
		{views: []string{"files", "photos"}, metaTTL: time.Second, dateZone: "UTC"},
		{views: nil, metaTTL: time.Second, dateZone: "UTC"},
		{views: vfs.DefaultViews, metaTTL: 0, dateZone: "UTC"},
		{views: vfs.DefaultViews, metaTTL: time.Second, dateZone: "Mars/Olympus"},
	} {
		if _, err := bad.viewOptions(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
