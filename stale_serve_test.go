package main

import (
	"testing"
)

func TestIsReasonixServeCmd(t *testing.T) {
	bin := "reasonix"
	if !isReasonixServeCmd("/usr/local/bin/reasonix serve --addr 127.0.0.1:23650", bin) {
		t.Fatal("expected serve match")
	}
	if isReasonixServeCmd("/usr/local/bin/reasonix doctor", bin) {
		t.Fatal("doctor should not match")
	}
}

func TestParseSSPIDs(t *testing.T) {
	sample := []byte(`LISTEN 0 4096 127.0.0.1:23650 0.0.0.0:* users:(("reasonix",pid=12345,fd=7))` + "\n" +
		`LISTEN 0 4096 127.0.0.1:23651 0.0.0.0:* users:(("reasonix",pid=12346,fd=10))`)
	got := parseSSPIDs(sample)
	if len(got) != 2 || got[0] != 12345 || got[1] != 12346 {
		t.Fatalf("parseSSPIDs = %v", got)
	}
}

func TestPersistedServePorts(t *testing.T) {
	dir := t.TempDir()
	st, err := newStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.upsert(chatRecord{ChatID: 99, Port: 23650}); err != nil {
		t.Fatal(err)
	}
	ports := persistedServePorts(st)
	if len(ports) != 1 {
		t.Fatalf("ports = %v", ports)
	}
	if _, ok := ports[23650]; !ok {
		t.Fatalf("want 23650 in %v", ports)
	}
}