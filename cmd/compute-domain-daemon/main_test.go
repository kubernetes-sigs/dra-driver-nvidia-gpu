package main

import "testing"

func set(items ...string) IPSet {
	m := make(IPSet, len(items))
	for _, i := range items {
		m[i] = struct{}{}
	}
	return m
}

func TestShouldSendSIGUSR1(t *testing.T) {
	tests := []struct {
		name  string
		old   IPSet
		new   IPSet
		fresh bool
		want  bool
	}{
		{
			name:  "no change",
			old:   set("A", "B", "C"),
			new:   set("A", "B", "C"),
			fresh: false,
			want:  false,
		},
		{
			name:  "no change",
			old:   set("A", "B", "C"),
			new:   set("A", "B", "C"),
			fresh: false,
			want:  false,
		},
		{
			name:  "pure removal",
			old:   set("A", "B", "C"),
			new:   set("A", "B"),
			fresh: false,
			want:  false,
		},
		{
			name:  "addition",
			old:   set("A", "B"),
			new:   set("A", "B", "C"),
			fresh: false,
			want:  true,
		},
		{
			name:  "replacement same size",
			old:   set("A", "B", "C"),
			new:   set("A", "B", "D"),
			fresh: false,
			want:  true,
		},
		{
			name:  "remove and add",
			old:   set("A", "B", "C"),
			new:   set("A", "D"),
			fresh: false,
			want:  true,
		},
		{
			name:  "fresh process",
			old:   set("A", "B"),
			new:   set("A", "B", "C"),
			fresh: true,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSendSIGUSR1(tt.old, tt.new, tt.fresh)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
