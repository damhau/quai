package azuredevops

import (
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	for raw, want := range map[string]Location{
		"https://devops.acme.ch/tfs/DefaultCollection/Infrastructure/_git/live":    {false, "https://devops.acme.ch/tfs/DefaultCollection", "Infrastructure", "live"},
		"https://devops.acme.ch/DefaultCollection/Infrastructure/_git/live":        {false, "https://devops.acme.ch/DefaultCollection", "Infrastructure", "live"},
		"https://devops.acme.ch:8443/tfs/Coll/_git/Infrastructure":                 {false, "https://devops.acme.ch:8443/tfs", "Coll", "Infrastructure"},
		"https://devops.acme.ch/Coll/_git/Infrastructure":                          {false, "https://devops.acme.ch/Coll", "Infrastructure", "Infrastructure"},
		"https://devops.acme.ch/tfs/Coll/Infra%20Live/_git/r%C3%A9seau":            {false, "https://devops.acme.ch/tfs/Coll", "Infra Live", "réseau"},
		"https://dev.azure.com/acme/Infrastructure/_git/live":                      {true, "https://dev.azure.com/acme", "Infrastructure", "live"},
		"https://acme@dev.azure.com/acme/Infrastructure/_git/live":                 {true, "https://dev.azure.com/acme", "Infrastructure", "live"},
		"https://dev.azure.com/acme/_git/Infrastructure":                           {true, "https://dev.azure.com/acme", "Infrastructure", "Infrastructure"},
		"https://acme.visualstudio.com/Infrastructure/_git/live":                   {true, "https://acme.visualstudio.com", "Infrastructure", "live"},
		"https://acme.visualstudio.com/DefaultCollection/Infrastructure/_git/live": {true, "https://acme.visualstudio.com", "Infrastructure", "live"},
	} {
		got, err := ParseURL(raw)
		if err != nil || got != want {
			t.Errorf("%s:\n got %+v, %v\nwant %+v", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"git@ssh.dev.azure.com:v3/acme/Infrastructure/live",
		"https://dev.azure.com/acme/Infrastructure/live",
		"https://dev.azure.com/acme/a/b/_git/live",
		"https://devops.acme.ch/_git/live",
		"https://dev.azure.com/acme/Infrastructure/_git/live/branches",
		"https://pat-secret@devops.acme.ch/live",
	} {
		if _, err := ParseURL(raw); err == nil {
			t.Errorf("%s was accepted", raw)
		} else if strings.Contains(err.Error(), "pat-secret") {
			t.Errorf("the error shows the URL's credentials: %v", err)
		}
	}
	if got := path("Infra Live", "_apis", "git", "repositories", "réseau"); got != "/Infra%20Live/_apis/git/repositories/r%C3%A9seau" {
		t.Errorf("path = %s", got)
	}
}
