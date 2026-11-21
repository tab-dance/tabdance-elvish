package vals

import (
	"errors"
	"strconv"
	"strings"

	"src.elv.sh/pkg/eval/errs"
)

var (
	errIndexMustBeInteger = errors.New("index must be integer")
)

func indexList(l List, rawIndex any) (any, error) {
	index, err := ConvertListIndex(rawIndex, l.Len())
	if err != nil {
		return nil, err
	}
	if index.Slice {
		return l.SubVector(index.Lower, index.Upper), nil
	}
	// Bounds are already checked.
	value, _ := l.Index(index.Lower)
	return value, nil
}

// ListIndex represents a (converted) list index.
type ListIndex struct {
	Slice bool
	Lower int
	Upper int
}

func adjustAndCheckIndex(i, n int, includeN bool) (int, error) {
	if i < 0 {
		if i < -n {
			return 0, negIndexOutOfRange(strconv.Itoa(i), n)
		}
		return i + n, nil
	}
	if includeN {
		if i > n {
			return 0, posIndexOutOfRange(strconv.Itoa(i), n+1)
		}
	} else {
		if i >= n {
			return 0, posIndexOutOfRange(strconv.Itoa(i), n)
		}
	}
	return i, nil
}

// ConvertListIndex parses a list index, check whether it is valid, and returns
// the converted structure.
func ConvertListIndex(rawIndex any, n int) (*ListIndex, error) {
	switch rawIndex := rawIndex.(type) {
	case int:
		index, err := adjustAndCheckIndex(rawIndex, n, false)
		if err != nil {
			return nil, err
		}
		return &ListIndex{false, index, 0}, nil
	case string:
		slice, i, j, sep, err := parseIndexString(rawIndex, n)
		if err != nil {
			return nil, err
		}
		if !slice {
			i, err = adjustAndCheckIndex(i, n, false)
			if err != nil {
				return nil, err
			}
		} else {
			i, err = adjustAndCheckIndex(i, n, true)
			if err != nil {
				return nil, err
			}
			j0 := j
			if sep == "..=" {
				j, err = adjustAndCheckClosedUpperIndex(j, i, n)
			} else {
				j, err = adjustAndCheckIndex(j, n, true)
			}
			if err != nil {
				return nil, err
			}
			if j < i {
				if j0 < 0 {
					return nil, errs.OutOfRange{
						What:     "negative slice upper index",
						ValidLow: strconv.Itoa(i - n), ValidHigh: "-1",
						Actual: strconv.Itoa(j0)}
				}
				return nil, errs.OutOfRange{
					What:     "slice upper index",
					ValidLow: strconv.Itoa(i), ValidHigh: strconv.Itoa(n),
					Actual: strconv.Itoa(j0)}
			}
		}
		return &ListIndex{slice, i, j}, nil
	default:
		return nil, errIndexMustBeInteger
	}
}

// Index = Number |
//
//	Number ( '..' | '..=' ) Number
func parseIndexString(s string, n int) (slice bool, i int, j int, sep string, err error) {
	low, sep, high := splitIndexString(s)
	if sep == "" {
		// A single number
		i, err := atoi(s, n)
		if err != nil {
			return false, 0, 0, "", err
		}
		return false, i, 0, "", nil
	}
	if low == "" {
		i = 0
	} else {
		i, err = atoi(low, n+1)
		if err != nil {
			return false, 0, 0, "", err
		}
	}
	if high == "" {
		if sep == "..=" {
			// -1 marks an omitted upper bound for a closed slice.
			j = -1
		} else {
			j = n
		}
	} else if sep == "..=" {
		j, err = atoiClosedUpper(high, n)
		if err != nil {
			return false, 0, 0, "", err
		}
	} else {
		j, err = atoi(high, n+1)
		if err != nil {
			return false, 0, 0, "", err
		}
	}
	// Two numbers
	return true, i, j, sep, nil
}

// adjustAndCheckClosedUpperIndex validates the upper bound of a closed slice
// (a..=b), which is inclusive, and converts it to an exclusive upper bound.
// The lower bound i has already been normalized.
func adjustAndCheckClosedUpperIndex(j, i, n int) (int, error) {
	if j == -1 {
		// -1 is the same as an omitted upper bound.
		return n, nil
	}
	if i == n {
		// The slice is already empty, so the only valid explicit upper bound
		// is -1 (handled above).
		return 0, errs.OutOfRange{
			What:     "closed slice upper index",
			ValidLow: "-1", ValidHigh: "-1",
			Actual: strconv.Itoa(j)}
	}
	if j < 0 {
		if j+n < i {
			return 0, errs.OutOfRange{
				What:     "closed slice upper index",
				ValidLow: strconv.Itoa(i - n), ValidHigh: "-1",
				Actual: strconv.Itoa(j)}
		}
		return j + n + 1, nil
	}
	if j >= n || j < i {
		return 0, errs.OutOfRange{
			What:     "closed slice upper index",
			ValidLow: strconv.Itoa(i), ValidHigh: strconv.Itoa(n - 1),
			Actual: strconv.Itoa(j)}
	}
	return j + 1, nil
}

func splitIndexString(s string) (low, sep, high string) {
	if i := strings.Index(s, "..="); i >= 0 {
		return s[:i], "..=", s[i+3:]
	}
	if i := strings.Index(s, ".."); i >= 0 {
		return s[:i], "..", s[i+2:]
	}
	return s, "", ""
}

// atoiClosedUpper is like atoi, but reports range errors in terms of a closed
// slice upper bound.
func atoiClosedUpper(a string, n int) (int, error) {
	i, err := strconv.Atoi(a)
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrRange {
			if i < 0 {
				return 0, errs.OutOfRange{
					What:     "closed slice upper index",
					ValidLow: strconv.Itoa(-n), ValidHigh: "-1",
					Actual: a}
			}
			return 0, errs.OutOfRange{
				What:     "closed slice upper index",
				ValidLow: "0", ValidHigh: strconv.Itoa(n - 1),
				Actual: a}
		}
		return 0, errIndexMustBeInteger
	}
	return i, nil
}

// atoi is a wrapper around strconv.Atoi, converting strconv.ErrRange to
// errs.OutOfRange.
func atoi(a string, n int) (int, error) {
	i, err := strconv.Atoi(a)
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrRange {
			if i < 0 {
				return 0, negIndexOutOfRange(a, n)
			}
			return 0, posIndexOutOfRange(a, n)
		}
		return 0, errIndexMustBeInteger
	}
	return i, nil
}

func posIndexOutOfRange(index string, n int) errs.OutOfRange {
	return errs.OutOfRange{
		What:     "index",
		ValidLow: "0", ValidHigh: strconv.Itoa(n - 1), Actual: index}
}

func negIndexOutOfRange(index string, n int) errs.OutOfRange {
	return errs.OutOfRange{
		What:     "negative index",
		ValidLow: strconv.Itoa(-n), ValidHigh: "-1", Actual: index}
}
