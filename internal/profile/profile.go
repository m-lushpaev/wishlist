package profile

type Profile struct {
	Slug     string
	Name     string
	Genitive string
}

var profiles = [...]Profile{{"maksim", "Максим", "Максима"}, {"anya", "Аня", "Ани"}, {"grisha", "Гриша", "Гриши"}, {"arisha", "Ариша", "Ариши"}}

func All() []Profile { return append([]Profile(nil), profiles[:]...) }

func Lookup(slug string) (Profile, bool) {
	for _, p := range profiles {
		if p.Slug == slug {
			return p, true
		}
	}
	return Profile{}, false
}
