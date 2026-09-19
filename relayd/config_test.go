package relayd

import "testing"

func TestComponentConfigValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Bundle: "b", DataDir: "d", Listeners: []Listener{{Listen: ":80", Backend: "web", Mode: "plain"}}}
	}
	ok := base()
	ok.Components = []ComponentConfig{{Name: "wscedge", Args: []string{"-port", "60115"}}}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid component refused: %v", err)
	}
	for name, comps := range map[string][]ComponentConfig{
		"empty name":    {{Name: ""}},
		"path":          {{Name: "../x"}},
		"upper case":    {{Name: "WscEdge"}},
		"hyphen":        {{Name: "wsc-edge"}},
		"relayd itself": {{Name: "relayd"}},
		"duplicate":     {{Name: "wscedge"}, {Name: "wscedge"}},
	} {
		c := base()
		c.Components = comps
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
