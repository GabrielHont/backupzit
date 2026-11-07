package imaging

import "fmt"

// selectDisks keeps request order and rejects missing, raw and duplicate disks.
// Explicit choices may include removable disks, like the single-disk UI.
func selectDisks(all []Disk, selection []int) ([]*Disk, error) {
	selected := make([]*Disk, 0, len(selection))
	seen := map[int]bool{}
	for _, number := range selection {
		if number < 0 || seen[number] {
			return nil, fmt.Errorf("invalid or duplicate disk %d", number)
		}
		seen[number] = true
		var found *Disk
		for i := range all {
			if all[i].Number == number {
				found = &all[i]
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("disk %d not found", number)
		}
		if found.Style == StyleRaw {
			return nil, fmt.Errorf("disk %d has no partition table", number)
		}
		selected = append(selected, found)
	}
	return selected, nil
}
