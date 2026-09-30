package lww

import (
	"testing"

	"github.com/dvher/pogo_pad/internal/model"
)

func TestWins(t *testing.T) {
	base := model.Note{UpdatedAt: 100, DeviceID: "b"}
	cases := []struct {
		name string
		in   model.Note
		want bool
	}{
		{"newer wins", model.Note{UpdatedAt: 101, DeviceID: "a"}, true},
		{"older loses", model.Note{UpdatedAt: 99, DeviceID: "z"}, false},
		{"tie higher device wins", model.Note{UpdatedAt: 100, DeviceID: "c"}, true},
		{"tie lower device loses", model.Note{UpdatedAt: 100, DeviceID: "a"}, false},
		{"identical loses", base, false},
	}
	for _, c := range cases {
		if got := Wins(c.in, base); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
