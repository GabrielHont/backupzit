package imaging

import "testing"

func TestSelectedDisks(t *testing.T) {
	all := []Disk{{Number: 0, Style: "gpt"}, {Number: 2, Style: "mbr"}, {Number: 3, Style: StyleRaw}}
	got, err := selectDisks(all, []int{2, 0})
	if err != nil || len(got) != 2 || got[0].Number != 2 || got[1].Number != 0 {
		t.Fatalf("selection: %+v %v", got, err)
	}
	for _, sel := range [][]int{{0, 0}, {-1}, {1}, {3}} {
		if _, err := selectDisks(all, sel); err == nil {
			t.Errorf("accepted %v", sel)
		}
	}
}
