package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestListSessionsReconcilesCompleteMuseCatalog(t *testing.T) {
	const retained = "47b11a3c-f1d7-4607-8744-cf09d866b4dc"
	const removed = "232d5a51-6611-4c9d-8c22-e9f60f310ea7"
	tests := []struct {
		name     string
		response string
		want     map[string]string
	}{
		{
			name:     "partial catalog keeps legacy entries",
			response: `{"path":"/","chats":[{"thread":"` + retained + `","title":"Renamed"}]}`,
			want:     map[string]string{retained: "Renamed", removed: "Legacy removed"},
		},
		{
			name:     "failed catalog does not prune cache",
			response: `{"error":"temporarily unavailable","path":"/","catalog_complete":true,"chats":[]}`,
			want:     map[string]string{retained: "Renamed", removed: "Legacy removed"},
		},
		{
			name:     "complete catalog prunes missing chat even on its stale route",
			response: `{"path":"/thread/` + removed + `","catalog_complete":true,"chats":[{"thread":"` + retained + `","title":"Renamed"}]}`,
			want:     map[string]string{retained: "Renamed"},
		},
		{
			name:     "partial catalog does not restore a removed chat",
			response: `{"path":"/thread/` + removed + `","chats":[{"thread":"` + removed + `","title":"Stale row"}]}`,
			want:     map[string]string{retained: "Renamed"},
		},
		{
			name:     "malformed complete response keeps cache",
			response: `{"path":"/","catalog_complete":true,"chats":[{"thread":"malformed","title":"Unknown"}]}`,
			want:     map[string]string{retained: "Renamed"},
		},
		{
			name:     "complete empty catalog prunes the last chat",
			response: `{"path":"/","catalog_complete":true,"chats":[]}`,
		},
		{
			name:     "restored chat reappears",
			response: `{"path":"/","catalog_complete":true,"chats":[{"thread":"` + removed + `","title":"Restored"}]}`,
			want:     map[string]string{removed: "Restored"},
		},
	}

	args := []string{"-c", `for response; do IFS= read -r request || exit; printf '%s\n' "$response"; done`, "muse-catalog-fixture"}
	for _, tt := range tests {
		args = append(args, tt.response)
	}
	worker := exec.Command("/bin/sh", args...)
	input, err := worker.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := worker.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	a := &adapter{
		web:       browser{command: worker, input: input, output: bufio.NewScanner(output)},
		statePath: filepath.Join(t.TempDir(), "sessions.json"),
		state: savedState{
			Chats: map[string]chat{
				retained: {Thread: retained, Title: "Legacy retained", FoundInMuse: true},
				removed:  {Thread: removed, Title: "Legacy removed", FoundInMuse: true},
			},
			Deliveries: map[string]delivery{"sent-request": {Thread: removed, Message: "native-message"}},
		},
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.save(); err != nil {
		t.Fatal(err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints, err := a.ListSessions(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(hints) != len(tt.want)+1 || hints[0].NativeThreadID != mainThread || !hints[0].Active {
				t.Fatalf("got sessions %+v, want active main and %v", hints, tt.want)
			}
			for _, hint := range hints[1:] {
				if tt.want[hint.NativeThreadID] != hint.Title || hint.Title == "" {
					t.Fatalf("unexpected side chat %+v, want %v", hint, tt.want)
				}
			}
			data, err := os.ReadFile(a.statePath)
			if err != nil {
				t.Fatal(err)
			}
			var stored savedState
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatal(err)
			}
			for thread, item := range stored.Chats {
				if item.FoundInMuse && tt.want[thread] != item.Title {
					t.Fatalf("persisted unexpected side chat %+v", item)
				}
			}
			if stored.Deliveries["sent-request"].Message != "native-message" {
				t.Fatal("catalog reconciliation lost the original delivery")
			}
		})
	}
}
