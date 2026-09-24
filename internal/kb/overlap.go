package kb

import "strings"

// PatternsOverlap reports whether two valid entity patterns can cover the same
// repository path. A pattern also covers descendants of a matched directory.
func PatternsOverlap(a, b string) bool {
	if CheckPattern(a) != nil || CheckPattern(b) != nil {
		return false
	}
	as := append(strings.Split(a, "/"), "**")
	bs := append(strings.Split(b, "/"), "**")
	seen := map[[2]int]bool{}
	var visit func(int, int) bool
	visit = func(i, j int) bool {
		if i == len(as) || j == len(bs) {
			return i == len(as) && j == len(bs)
		}
		key := [2]int{i, j}
		if seen[key] {
			return false
		}
		seen[key] = true
		if as[i] == "**" && visit(i+1, j) || bs[j] == "**" && visit(i, j+1) {
			return true
		}
		switch {
		case as[i] == "**" && bs[j] == "**":
			return true
		case as[i] == "**":
			return visit(i, j+1)
		case bs[j] == "**":
			return visit(i+1, j)
		default:
			return segmentsOverlap(as[i], bs[j]) && visit(i+1, j+1)
		}
	}
	return visit(0, 0)
}

// segmentsOverlap intersects two path.Match segment languages. Literal bytes
// and character classes are converted to transitions; stars have an epsilon
// edge and a transition back to themselves.
func segmentsOverlap(a, b string) bool {
	aa, bb := globTokens(a), globTokens(b)
	seen := map[[2]int]bool{}
	var visit func(int, int) bool
	visit = func(i, j int) bool {
		if i == len(aa) || j == len(bb) {
			if i == len(aa) && j == len(bb) {
				return true
			}
			if i < len(aa) && aa[i].star {
				return visit(i+1, j)
			}
			if j < len(bb) && bb[j].star {
				return visit(i, j+1)
			}
			return false
		}
		key := [2]int{i, j}
		if seen[key] {
			return false
		}
		seen[key] = true
		if aa[i].star && visit(i+1, j) || bb[j].star && visit(i, j+1) {
			return true
		}
		if rangesIntersect(aa[i].ranges, bb[j].ranges) {
			ni, nj := i+1, j+1
			if aa[i].star {
				ni = i
			}
			if bb[j].star {
				nj = j
			}
			return visit(ni, nj)
		}
		return false
	}
	return visit(0, 0)
}

type runeRange struct{ lo, hi rune }
type globToken struct {
	ranges []runeRange
	star   bool
}

func globTokens(pattern string) []globToken {
	var tokens []globToken
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			tokens = append(tokens, globToken{ranges: []runeRange{{1, '/' - 1}, {'/' + 1, 0x10ffff}}, star: true})
		case '?':
			tokens = append(tokens, globToken{ranges: []runeRange{{1, '/' - 1}, {'/' + 1, 0x10ffff}}})
		case '\\':
			i++
			tokens = append(tokens, globToken{ranges: []runeRange{{runes[i], runes[i]}}})
		case '[':
			j := i + 1
			negated := j < len(runes) && runes[j] == '^'
			if negated {
				j++
			}
			var ranges []runeRange
			for j < len(runes) && runes[j] != ']' {
				lo := runes[j]
				if lo == '\\' {
					j++
					lo = runes[j]
				}
				hi := lo
				j++
				if j < len(runes) && runes[j] == '-' {
					j++
					hi = runes[j]
					if hi == '\\' {
						j++
						hi = runes[j]
					}
					j++
				}
				ranges = append(ranges, runeRange{lo, hi})
			}
			if negated {
				ranges = complement(ranges)
			}
			tokens = append(tokens, globToken{ranges: ranges})
			i = j
		default:
			tokens = append(tokens, globToken{ranges: []runeRange{{runes[i], runes[i]}}})
		}
	}
	return tokens
}

func rangesIntersect(a, b []runeRange) bool {
	for _, x := range a {
		for _, y := range b {
			if x.lo <= y.hi && y.lo <= x.hi && x.lo <= x.hi && y.lo <= y.hi {
				return true
			}
		}
	}
	return false
}

func complement(excluded []runeRange) []runeRange {
	var out []runeRange
	for _, domain := range []runeRange{{1, '/' - 1}, {'/' + 1, 0x10ffff}} {
		start := domain.lo
		for start <= domain.hi {
			end := domain.hi
			for _, r := range excluded {
				if r.lo <= start && start <= r.hi {
					start = r.hi + 1
					end = -1
					break
				}
				if r.lo > start && r.lo-1 < end {
					end = r.lo - 1
				}
			}
			if end >= start {
				out = append(out, runeRange{start, end})
				start = end + 1
			}
		}
	}
	return out
}
