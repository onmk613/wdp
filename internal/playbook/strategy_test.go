package playbook

import "testing"

func TestParseStrategy(t *testing.T) {
	plays, err := Parse([]byte(`
- name: 分批部署
  hosts: webservers
  strategy:
    type: canary
    batch: "10%"
    gate:
      shell: 'curl -sf http://localhost:8080/health'
      retries: 10
      delay: 3
    auto_rollback: true
  tasks:
    - shell: 'true'
`))
	if err != nil {
		t.Fatal(err)
	}
	st := plays[0].Strategy
	if st == nil {
		t.Fatal("strategy 未解析")
	}
	if st.Type != "canary" || st.Batch != "10%" || !st.AutoRollback {
		t.Fatalf("%+v", st)
	}
	if st.Gate == nil {
		t.Fatal("gate 未解析")
	}
	if st.Gate.FreeForm == "" || st.Gate.Retries != 10 || st.Gate.DelaySec != 3 {
		t.Fatalf("gate: %+v", st.Gate)
	}
	// 缺省 until = rc==0 判据
	if st.Gate.Until != `{{ if eq .result.rc 0 }}ok{{ end }}` {
		t.Fatalf("gate until 缺省值: %q", st.Gate.Until)
	}
}

func TestParseStrategyErrors(t *testing.T) {
	cases := []string{
		`- hosts: all
  strategy: {type: fast}
  tasks: [{shell: x}]`,
		`- hosts: all
  strategy: {type: rolling, gate: {until: x}}
  tasks: [{shell: x}]`,
		`- hosts: all
  strategy: "rolling"
  tasks: [{shell: x}]`,
	}
	for i, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Fatalf("用例 %d 应报错", i+1)
		}
	}
}

func TestParseStrategyDefaults(t *testing.T) {
	plays, err := Parse([]byte(`
- hosts: all
  strategy: {type: rolling}
  tasks: [{shell: x}]
`))
	if err != nil {
		t.Fatal(err)
	}
	st := plays[0].Strategy
	if st.Batch != "" || st.Gate != nil || st.AutoRollback {
		t.Fatalf("%+v", st)
	}
}
