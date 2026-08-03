package miniagent

import (
	"testing"
)

// scrubEnv 除 MINIAGENT_ 前缀外，还需剥离变量名含 KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/
// PWD/PASS/PASSPHRASE/AUTH 的条目（P2-8 + 三轮 P2 关键字表补）：覆盖 config ${MAIN_API_KEY}
// 注入来源、AWS_ACCESS_KEY_ID、GH_TOKEN、MYSQL_PWD、DB_PASS/REDIS_PASS、GPG_PASSPHRASE、
// BASIC_AUTH/AUTH_HEADER 等。DATABASE_URL 等无关键字变量保留；含关键字子串的普通变量
// （MONKEY/TOKEN_BUCKET/KEYMAP/AUTHPROXY）会过度剥离——安全侧倾斜的已知取舍，由 scrubEnv 注释明示。
func TestScrubEnv(t *testing.T) {
	cases := []struct {
		kv   string
		keep bool
	}{
		{"MINIAGENT_API_KEY=sk-mini", false},  // 前缀剥离
		{"MAIN_API_KEY=sk-injected", false},   // ${MAIN_API_KEY} 注入来源，含 KEY
		{"GH_TOKEN=ghp_xxx", false},           // 含 TOKEN
		{"AWS_ACCESS_KEY_ID=AKIAxxx", false},  // 含 KEY
		{"DB_PASSWORD=pw", false},             // 含 PASSWORD（亦含 PASS）
		{"MY_CREDENTIAL_FILE=/x", false},      // 含 CREDENTIAL
		{"TOPSECRET=v", false},                // 含 SECRET
		{"MYSQL_PWD=secret", false},           // 含 PWD（三轮 P2 关键字表补）
		{"DB_PASS=secret", false},             // 含 PASS
		{"REDIS_PASS=secret", false},          // 含 PASS
		{"GPG_PASSPHRASE=secret", false},      // 含 PASSPHRASE（亦含 PASS）
		{"BASIC_AUTH=secret", false},          // 含 AUTH
		{"AUTH_HEADER=Bearer x", false},       // 含 AUTH
		{"TOKEN_BUCKET=100", false},           // 含 TOKEN → 误伤（过度剥离）
		{"MONKEY_COUNT=5", false},             // 含 KEY → 误伤（过度剥离）
		{"KEYMAP=us", false},                  // 含 KEY → 误伤（键盘布局，非密钥）
		{"AUTHPROXY=http://corp:3128", false}, // 含 AUTH → 误伤（短关键字扩大误伤面，已知取舍）
		{"DATABASE_URL=postgres://db", true},  // 无关键字 → 保留
		{"PATH=/usr/bin:/bin", true},          // 保留
		{"HOME=/root", true},                  // 保留
		{"LANG=en_US.UTF-8", true},            // 保留
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

// hasSecretKeyword 直接覆盖（第五轮 P3）：PAT 命中 GITHUB_PAT/GITLAB_PAT/AZURE_DEVOPS_EXT_PAT
// 等 fine-grained token；PATH 族（PATH/PATHEXT/LD_LIBRARY_PATH/CPATH/GITHUB_PATH）含 PATH 必
// 豁免——PATH 误剥会让 shell 找不到 ls/grep。MONKEY_COUNT/AUTHPROXY/COMPAT_MODE 等误伤须仍
// true（安全侧倾斜，与 TestScrubEnv 一致）。
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
