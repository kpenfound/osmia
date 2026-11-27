package verification

import "testing"

func TestZeroSettingIsDisabledAndValid(t *testing.T) {
	out, field, reason := Setting{}.Validate()
	if field != "" || reason != "" || out.Enabled() || out.MapDir != "" {
		t.Fatalf("%+v %q %q", out, field, reason)
	}
	if (Setting{}).Directory() != "docs/verification" {
		t.Fatal("default directory")
	}
}

func TestValidateDefaultsTheMapDirectoryAndNamesTheField(t *testing.T) {
	out, field, _ := Setting{Call: []string{"lever"}}.Validate()
	if field != "" || !out.Enabled() || out.MapDir != DefaultMapDir {
		t.Fatalf("%+v %q", out, field)
	}
	for name, tc := range map[string]struct {
		s     Setting
		field string
	}{
		"no call":   {Setting{MapDir: "docs/x"}, "call"},
		"bad call":  {Setting{Call: []string{"lever", "-x"}}, "call"},
		"run id":    {Setting{Call: []string{"lever", "--run-id=1"}}, "call"},
		"abs dir":   {Setting{Call: []string{"lever"}, MapDir: "/docs"}, "map_dir"},
		"up dir":    {Setting{Call: []string{"lever"}, MapDir: "../docs"}, "map_dir"},
		"unclean":   {Setting{Call: []string{"lever"}, MapDir: "docs//x"}, "map_dir"},
		"dot dir":   {Setting{Call: []string{"lever"}, MapDir: "."}, "map_dir"},
		"up glob":   {Setting{Call: []string{"lever"}, LeverPaths: []string{"a/../../b"}}, "lever_paths"},
		"abs glob":  {Setting{Call: []string{"lever"}, LeverPaths: []string{"/a"}}, "lever_paths"},
		"bad glob":  {Setting{Call: []string{"lever"}, LeverPaths: []string{"a/["}}, "lever_paths"},
		"empty one": {Setting{Call: []string{"lever"}, LeverPaths: []string{""}}, "lever_paths"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, field, reason := tc.s.Validate(); field != tc.field || reason == "" {
				t.Fatalf("field %q reason %q", field, reason)
			}
		})
	}
}
