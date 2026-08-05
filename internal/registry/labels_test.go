package registry

import (
	"reflect"
	"testing"
)

func TestDeriveLabels(t *testing.T) {
	got := DeriveLabels("simulator", "iOS 26.5", "iPhone 17 Pro")
	want := []string{"simulator", "ios26", "ios26.5", "iphone-17-pro"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeriveLabelsNoMinor(t *testing.T) {
	got := DeriveLabels("simulator", "iOS 18", "iPad Pro 13-inch (M4)")
	want := []string{"simulator", "ios18", "ipad-pro-13-inch-m4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
