package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchCredentialSources(t *testing.T) {
	for _, tc := range []struct{ name, explicit, stored, bad, want string }{
		{name: "provider_only"},
		{name: "explicit", explicit: "sr-synthetic-explicit", want: "sr-synthetic-explicit"},
		{name: "stored", stored: "sr-synthetic-stored", want: "sr-synthetic-stored"},
		{name: "override", explicit: "sr-synthetic-explicit", stored: "sr-synthetic-stored", want: "sr-synthetic-explicit"},
		{name: "corrupt", bad: "not json"},
		{name: "readable", stored: "sr-synthetic-stored"},
		{name: "symlink", stored: "sr-synthetic-stored"},
		{name: "invalid", bad: `{"v":1,"api_key":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(routerBody)) })
			path := filepath.Join(root, credentialsFile)
			if tc.stored != "" {
				if err := writeCredentials(path, credentials{APIKey: tc.stored}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bad != "" {
				if err := os.WriteFile(path, []byte(tc.bad), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "readable" {
				if !posixModes {
					t.Skip("POSIX mode check")
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				} // #nosec G302 -- tests the existing refusal of a readable credentials file
			}
			if tc.name == "symlink" {
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			_, existingErr := readCredentials(path)
			code, _, errOut := runSearch(t, fixedSearchOps(root), map[string]string{"OPENAI_API_KEY": "sk-synthetic-provider", apiKeyEnv: tc.explicit}, "-config", cfg, "query")
			fr.mu.Lock()
			defer fr.mu.Unlock()
			if tc.want == "" {
				if len(fr.reqs) != 0 {
					t.Fatalf("router received %d requests; want zero", len(fr.reqs))
				}
				if code != exitClientErr {
					t.Fatalf("exit %d; want missing/invalid credential exit %d", code, exitClientErr)
				}
				if tc.bad != "" || tc.name == "readable" || tc.name == "symlink" {
					if existingErr == nil || !strings.Contains(errOut, existingErr.Error()) {
						t.Fatalf("existing credential error not preserved: %q", errOut)
					}
				} else if !strings.Contains(errOut, "dropin-miner login") {
					t.Fatalf("missing recovery guidance: %q", errOut)
				}
			} else if code != exitOK || len(fr.reqs) != 1 || fr.reqs[0].Header.Get("Authorization") != "Bearer "+tc.want {
				t.Fatalf("credential routing mismatch: code=%d requests=%d", code, len(fr.reqs))
			}
		})
	}
}
