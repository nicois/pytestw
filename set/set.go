package set

type (
	Void      struct{}
	StringSet map[string]Void
)

var Member Void

func Create(strings ...string) StringSet {
	result := make(StringSet)
	for _, s := range strings {
		result[s] = Member
	}
	return result
}

func (p *StringSet) Add(items ...string) *StringSet {
	for _, i := range items {
		(*p)[i] = Member
	}
	return p
}

func (p *StringSet) Discard(other StringSet) *StringSet {
	for o := range other {
		delete(*p, o)
	}
	return p
}

func (p *StringSet) Union(other StringSet) *StringSet {
	for o := range other {
		(*p)[o] = Member
	}
	return p
}

func (p *StringSet) Intersection(other StringSet) *StringSet {
	for mine := range *p {
		if _, exists := other[mine]; !exists {
			delete(*p, mine)
		}
	}
	return p
}

func (s *StringSet) Copy() StringSet {
	result := make(StringSet)
	for o := range *s {
		result[o] = Member
	}
	return result
}
