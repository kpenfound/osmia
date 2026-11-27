package verification

import (
	"slices"
	"testing"
	"testing/fstest"
)

func pointerTree() fstest.MapFS {
	tree := newMap()
	tree["internal/cart/cart.go"] = &fstest.MapFile{Data: []byte("package cart")}
	tree["internal/cart/price/price.go"] = &fstest.MapFile{Data: []byte("package price")}
	tree["internal/admin/admin.go"] = &fstest.MapFile{Data: []byte("package admin")}
	tree["internal/other/other.go"] = &fstest.MapFile{Data: []byte("package other")}
	return tree
}

func TestApplicableNamesTheFeatureOfAnExistingFileAFootprintPatternReaches(t *testing.T) {
	tree := pointerTree()
	m := ReadMap(tree, "docs/verification")
	got := m.Applicable(tree, []string{"internal/cart/**"}, nil)
	if !slices.Equal(names(got), []string{"Checkout"}) {
		t.Fatalf("%v", names(got))
	}
}

func TestApplicableNamesTheFeatureOfADirectoryThatDoesNotExistYetByLiteralPrefix(t *testing.T) {
	tree := pointerTree()
	m := ReadMap(tree, "docs/verification")
	got := m.Applicable(tree, []string{"internal/cart/newpkg/**"}, nil)
	if !slices.Equal(names(got), []string{"Checkout"}) {
		t.Fatalf("%v", names(got))
	}
}

func TestApplicableNamesNoFeatureForAFootprintWithNoOverlap(t *testing.T) {
	tree := pointerTree()
	m := ReadMap(tree, "docs/verification")
	for _, patterns := range [][]string{{"internal/other/**"}, {"docs/unrelated/**"}, {"cmd/tool/**"}, nil} {
		if got := m.Applicable(tree, patterns, nil); len(got) != 0 {
			t.Fatalf("%v names %v", patterns, names(got))
		}
	}
}

func TestApplicableNamesEveryFeatureForTheWholeRepository(t *testing.T) {
	tree := pointerTree()
	m := ReadMap(tree, "docs/verification")
	if got := m.Applicable(tree, []string{"**"}, nil); len(got) != 2 {
		t.Fatalf("%v", names(got))
	}
}

func TestApplicableAddsFeaturesOfChangedFilesAndKeepsMapOrder(t *testing.T) {
	tree := pointerTree()
	m := ReadMap(tree, "docs/verification")
	got := m.Applicable(tree, []string{"internal/other/**"}, []string{"internal/cart/price/price.go", "internal/admin/new.go"})
	if !slices.Equal(names(got), []string{"Admin", "Checkout"}) {
		t.Fatalf("%v", names(got))
	}
}

func TestApplicableWithoutATreeFallsBackToOverlap(t *testing.T) {
	m := ReadMap(pointerTree(), "docs/verification")
	got := m.Applicable(nil, []string{"internal/admin/**"}, nil)
	if !slices.Equal(names(got), []string{"Admin"}) {
		t.Fatalf("%v", names(got))
	}
}
