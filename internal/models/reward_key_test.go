package models

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
)

func TestNormalizeRewardKeyInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "canonical", input: "g1::cool skin", want: "g1::cool skin"},
		{name: "compatible case and outer space", input: " G1::Cool Skin ", want: "g1::cool skin"},
		{name: "empty components", input: "::", want: "::"},
		{name: "missing game", input: "::reward", want: "::reward"},
		{name: "missing name", input: "game::", want: "game::"},
		{name: "embedded delimiters", input: "a::b::c", want: "a::b::c"},
		{name: "overlapping delimiters", input: "g1::::name", want: "g1::::name"},
		{name: "delimiter whitespace", input: "G1 :: Cool Skin", wantErr: true},
		{name: "component whitespace", input: "g1:: cool skin", wantErr: true},
		{name: "no delimiter", input: "orphan", wantErr: true},
		{name: "blank", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeRewardKeyInput(tt.input)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidRewardKey) {
					t.Fatalf("NormalizeRewardKeyInput(%q) error = %v, want ErrInvalidRewardKey", tt.input, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("NormalizeRewardKeyInput(%q) = %q, %v; want %q, nil", tt.input, got, err, tt.want)
			}
		})
	}
}

func TestNormalizeRewardKeyInputAcceptsEveryOwnerOutput(t *testing.T) {
	property := func(gameID, name string) bool {
		ownerKey := NormalizeRewardKey(gameID, name)
		got, err := NormalizeRewardKeyInput(ownerKey)
		return err == nil && got == ownerKey
	}
	if err := quick.Check(property, &quick.Config{
		MaxCount: 1000,
		Rand:     rand.New(rand.NewSource(1)),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeRewardKeyInputBoundsAdversarialDelimiterInput(t *testing.T) {
	// Every delimiter is surrounded by component-edge whitespace, so there is
	// no valid owner boundary. This shape forced the former implementation to
	// rebuild the whole candidate once per delimiter (quadratic work).
	malformed := strings.Repeat("component :: ", 50_000) + "tail"
	if _, err := NormalizeRewardKeyInput(malformed); !errors.Is(err, ErrInvalidRewardKey) {
		t.Fatalf("adversarial delimiter input error = %v, want ErrInvalidRewardKey", err)
	}

	overLimit := strings.Repeat("x", maxRewardKeyInputBytes+1)
	if _, err := NormalizeRewardKeyInput(overLimit); !errors.Is(err, ErrInvalidRewardKey) {
		t.Fatalf("over-limit input error = %v, want ErrInvalidRewardKey", err)
	}
}
