package hiveslack

import (
	"encoding/json"
	"testing"

	"github.com/Hive-RSI-Lab/hive-slack/internal/bridge"
)

// The manifest asks for every scope managing channels takes.
func TestManifestScopes(t *testing.T) {
	var m struct {
		OAuth struct {
			Scopes struct{ Bot []string } `json:"scopes"`
		} `json:"oauth_config"`
	}
	if err := json.Unmarshal([]byte(Manifest), &m); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range m.OAuth.Scopes.Bot {
		have[s] = true
	}
	for _, s := range bridge.ManageScopes {
		if !have[s] {
			t.Errorf("the manifest lacks %s", s)
		}
	}
}
