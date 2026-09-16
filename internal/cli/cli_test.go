package cli

import (
	"reflect"
	"testing"
)

func TestCountLabels(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]int
		wantErr bool
	}{
		{"empty output", "", map[string]int{}, false},
		{"no issues", "[]", map[string]int{}, false},
		{"counts across issues", `[{"labels":[{"name":"agent-ready"},{"name":"wave:ui"}]},{"labels":[{"name":"agent-ready"}]},{"labels":[]}]`,
			map[string]int{"agent-ready": 2, "wave:ui": 1}, false},
		{"garbage", "not json", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := countLabels(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDependsBody(t *testing.T) {
	got := dependsBody("do the thing", []string{"A", "B"}, map[string]string{"A": "3", "B": "7"})
	want := "do the thing\n\n## Depends on\n- #3\n- #7\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
