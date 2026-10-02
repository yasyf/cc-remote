package images

import "slices"

var tailnetBins = []string{"tailscale", "tailscaled"}

func (inv Inventory) TailnetFromTools() bool {
	for _, bin := range tailnetBins {
		static := slices.ContainsFunc(inv.System, func(a Artifact) bool {
			_, ok := a.bins()[bin]
			return ok && a.Format != Deb
		})
		if !static {
			return false
		}
	}
	return true
}
