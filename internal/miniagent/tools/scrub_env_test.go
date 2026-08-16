package tools

import (
	"strings"
	"testing"
)

// scrubEnv, in addition to MINIAGENT_ prefix, also strips entries whose variable names contain KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/
// PWD/PASS/PASSPHRASE/AUTH (P2-8 + three rounds of P2 keyword table additions): covers config ${MAIN_API_KEY}
// injection source, AWS_ACCESS_KEY_ID, GH_TOKEN, MYSQL_PWD, DB_PASS/REDIS_PASS, GPG_PASSPHRASE,
// BASIC_AUTH/AUTH_HEADER, etc. Non-keyword variables like DATABASE_URL are kept; normal variables containing
// keyword substrings (MONKEY/TOKEN_BUCKET/KEYMAP/AUTHPROXY) are over-stripped — a known trade-off leaning
// toward the security side, documented in the scrubEnv comment.
func TestScrubEnv(t *testing.T) {
	cases := []struct {
		kv   string
		keep bool
	}{
		{"MINIAGENT_API_KEY=sk-mini", false},  // prefix stripped
		{"MAIN_API_KEY=sk-injected", false},   // ${MAIN_API_KEY} injection source, contains KEY
		{"GH_TOKEN=ghp_xxx", false},           // contains TOKEN
		{"AWS_ACCESS_KEY_ID=AKIAxxx", false},  // contains KEY
		{"DB_PASSWORD=pw", false},             // contains PASSWORD (also contains PASS)
		{"MY_CREDENTIAL_FILE=/x", false},      // contains CREDENTIAL
		{"TOPSECRET=v", false},                // contains SECRET
		{"MYSQL_PWD=secret", false},           // contains PWD (three rounds of P2 keyword table addition)
		{"DB_PASS=secret", false},             // contains PASS
		{"REDIS_PASS=secret", false},          // contains PASS
		{"GPG_PASSPHRASE=secret", false},      // contains PASSPHRASE (also contains PASS)
		{"BASIC_AUTH=secret", false},          // contains AUTH
		{"AUTH_HEADER=Bearer x", false},       // contains AUTH
		{"TOKEN_BUCKET=100", false},           // contains TOKEN → false positive (over-stripped)
		{"MONKEY_COUNT=5", false},             // contains KEY → false positive (over-stripped)
		{"KEYMAP=us", false},                  // contains KEY → false positive (keyboard layout, not a secret)
		{"AUTHPROXY=http://corp:3128", false}, // contains AUTH → false positive (short keyword widens false positive surface, known trade-off)
		{"DATABASE_URL=postgres://db", true},  // no keyword → kept
		{"PATH=/usr/bin:/bin", true},          // kept
		{"HOME=/root", true},                  // kept
		{"LANG=en_US.UTF-8", true},            // kept
	}
	in := make([]string, len(cases))
	for i, c := range cases {
		in[i] = c.kv
	}
	out := scrubEnv(in)
	have := make(map[string]bool, len(out))
	for _, kv := range out {
		have[kv] = true
	}
	for _, c := range cases {
		if c.keep && !have[c.kv] {
			t.Errorf("scrubEnv should keep %q", c.kv)
		}
		if !c.keep && have[c.kv] {
			t.Errorf("scrubEnv should strip %q", c.kv)
		}
	}
}

// hasSecretKeyword direct coverage (round 5 P3): PAT matches GITHUB_PAT/GITLAB_PAT/AZURE_DEVOPS_EXT_PAT
// and other fine-grained tokens; the PATH family (PATH/PATHEXT/LD_LIBRARY_PATH/CPATH/GITHUB_PATH) contains PATH
// and must be exempted — stripping PATH would break shell's ability to find ls/grep. MONKEY_COUNT/AUTHPROXY/COMPAT_MODE
// and other false positives must still return true (leaning toward the security side, consistent with TestScrubEnv).
func TestHasSecretKeyword(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"GITHUB_PAT", true},
		{"GITLAB_PAT", true},
		{"AZURE_DEVOPS_EXT_PAT", true},
		{"GH_TOKEN", true},
		{"MAIN_API_KEY", true},
		{"MYSQL_PWD", true},
		{"MONKEY_COUNT", true},
		{"AUTHPROXY", true},
		{"COMPAT_MODE", true},
		{"PATH", false},
		{"PATHEXT", false},
		{"LD_LIBRARY_PATH", false},
		{"CPATH", false},
		{"GITHUB_PATH", false},
		{"DATABASE_URL", false},
		{"HOME", false},
		{"LANG", false},
	}
	for _, c := range cases {
		if got := hasSecretKeyword(c.name); got != c.want {
			t.Errorf("hasSecretKeyword(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// restoreGitCredentials：push/pull 需要的远端凭证（https token / ssh agent）从原环境回补到已 scrub 的 env，
// 其余 secret 命名变量不回流；MINIAGENT_* 永不回流。
func TestRestoreGitCredentials(t *testing.T) {
	raw := []string{
		"GITHUB_TOKEN=ghp_x", "SSH_AUTH_SOCK=/tmp/agent.sock", "MINIAGENT_API_KEY=sk-mini",
		"AWS_ACCESS_KEY_ID=AKIA", "PATH=/usr/bin",
	}
	scrubbed := scrubEnv(raw)
	got := restoreGitCredentials(scrubbed, raw)
	have := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		have[k] = v
	}
	if have["GITHUB_TOKEN"] != "ghp_x" {
		t.Errorf("GITHUB_TOKEN should be restored for push/pull, got env: %v", got)
	}
	if have["SSH_AUTH_SOCK"] != "/tmp/agent.sock" {
		t.Errorf("SSH_AUTH_SOCK should be restored for push/pull, got env: %v", got)
	}
	for _, banned := range []string{"MINIAGENT_API_KEY", "AWS_ACCESS_KEY_ID"} {
		if _, ok := have[banned]; ok {
			t.Errorf("%s must NOT be restored", banned)
		}
	}
}
