package semantic

import "math"

// Usage is how a set of paragraphs reads taken together - the sum of
// their vectors. Two of them are compared with Alike: the paragraphs that
// write one name and the paragraphs that write another, to tell whether a
// book uses the two the same way. The zero value is an empty one.
type Usage struct {
	sum []float64
	n   int
}

// N is how many paragraphs u holds.
func (u *Usage) N() int { return u.n }

func (u *Usage) add(vec []int8) {
	if u.sum == nil {
		u.sum = make([]float64, len(vec))
	}
	if len(vec) != len(u.sum) {
		return // built at another width
	}
	for i, x := range vec {
		u.sum[i] += float64(x)
	}
	u.n++
}

// AddParagraphs adds to u those of bookID's paragraphs ids the index has a
// vector for (short paragraphs have none, nor has a book not yet
// embedded).
func (s *Service) AddParagraphs(u *Usage, bookID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	bv, err := s.vectors(bookID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if i, ok := bv.row(id); ok {
			u.add(bv.data[i*bv.dim : (i+1)*bv.dim])
		}
	}
	return nil
}

// AddBook adds every paragraph of bookID to u: what Alike compares two
// usages against.
func (s *Service) AddBook(u *Usage, bookID string) error {
	bv, err := s.vectors(bookID)
	if err != nil {
		return err
	}
	for i := range bv.ids {
		u.add(bv.data[i*bv.dim : (i+1)*bv.dim])
	}
	return nil
}

// Alike is how alike a and b read, from -1 to 1: the cosine of their mean
// vectors, each taken from among's mean first - any two sets of a book's
// paragraphs are close before that, since all of it is the one story. 0
// when any of the three is empty.
func Alike(a, b, among *Usage) float64 {
	if a.n == 0 || b.n == 0 || among.n == 0 || len(a.sum) != len(b.sum) || len(a.sum) != len(among.sum) {
		return 0
	}
	var ab, aa, bb float64
	for i := range a.sum {
		m := among.sum[i] / float64(among.n)
		x, y := a.sum[i]/float64(a.n)-m, b.sum[i]/float64(b.n)-m
		ab += x * y
		aa += x * x
		bb += y * y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}
