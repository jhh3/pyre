package extensions

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Preview is an adoption preview: what Pyre will reference in place.
type Preview struct {
	Counts    map[Kind]int          `json:"counts"`
	Managed   int                   `json:"managed"`
	Unmanaged int                   `json:"unmanaged"`
	Items     []Item                `json:"items"`
	Conflicts []contracts.DriftItem `json:"conflicts,omitempty"`
	Warnings  []string              `json:"warnings,omitempty"`
	Summary   string                `json:"summary"`
}

// AdoptionPreview summarizes an inventory for adoption. Adoption references
// existing items where they are; it never moves, copies or rewrites them.
func AdoptionPreview(inv Inventory) Preview {
	p := Preview{Counts: map[Kind]int{}, Items: inv.Items, Conflicts: inv.Duplicates, Warnings: inv.Warnings}
	for _, it := range inv.Items {
		p.Counts[it.Kind]++
		if it.Managed {
			p.Managed++
		} else {
			p.Unmanaged++
		}
	}
	kinds := make([]string, 0, len(p.Counts))
	for k, n := range p.Counts {
		kinds = append(kinds, fmt.Sprintf("%d %s", n, plural(string(k), n)))
	}
	sort.Strings(kinds)
	total := len(inv.Items)
	p.Summary = fmt.Sprintf("These %d items will be referenced, nothing moved (%s).", total, strings.Join(kinds, ", "))
	if len(p.Conflicts) > 0 {
		p.Summary += fmt.Sprintf(" %d duplicate registration(s) already exist and are reported, not changed.", len(p.Conflicts))
	}
	return p
}

func plural(s string, n int) string {
	if n == 1 {
		return s
	}
	return s + "s"
}
