package main

import "testing"

func TestParsePublish(t *testing.T) {
	tests := []struct {
		raw     string
		want    publishSpec
		wantErr bool
	}{
		{raw: "9000=127.0.0.1:22", want: publishSpec{port: 9000, target: "127.0.0.1:22"}},
		{raw: ":9000=localhost:22", want: publishSpec{port: 9000, target: "localhost:22"}},
		{raw: "19020=127.0.0.1:18080", want: publishSpec{port: 19020, target: "127.0.0.1:18080"}},
		{raw: "9000=127.0.0.1", wantErr: true},
		{raw: "0=127.0.0.1:22", wantErr: true},
		{raw: "70000=127.0.0.1:22", wantErr: true},
		{raw: "abc=127.0.0.1:22", wantErr: true},
		{raw: "9000", wantErr: true},
		{raw: "=127.0.0.1:22", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parsePublish(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parsePublish(%q) = %+v, want error", tt.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePublish(%q): %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parsePublish(%q) = %+v, want %+v", tt.raw, got, tt.want)
		}
	}
}

func TestParseMap(t *testing.T) {
	tests := []struct {
		raw     string
		want    mapSpec
		wantErr bool
	}{
		{raw: "127.0.0.1:2222=100.64.0.21:22", want: mapSpec{local: "127.0.0.1:2222", target: "100.64.0.21:22"}},
		{raw: "127.0.0.1:8080=100.64.0.30:80", want: mapSpec{local: "127.0.0.1:8080", target: "100.64.0.30:80"}},
		{raw: "127.0.0.1=100.64.0.21:22", wantErr: true},
		{raw: "127.0.0.1:2222=100.64.0.21", wantErr: true},
		{raw: "127.0.0.1:2222", wantErr: true},
		{raw: "=100.64.0.21:22", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseMap(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMap(%q) = %+v, want error", tt.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMap(%q): %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMap(%q) = %+v, want %+v", tt.raw, got, tt.want)
		}
	}
}
