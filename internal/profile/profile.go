package profile

type Profile struct {
	Slug string
	Name string
}

var profiles = [...]Profile{{"maksim", "Максим"}, {"anya", "Аня"}, {"grisha", "Гриша"}, {"arisha", "Ариша"}}

func All() []Profile { return append([]Profile(nil), profiles[:]...) }

func Lookup(slug string) (Profile, bool) {
	for _, p := range profiles {
		if p.Slug == slug {
			return p, true
		}
	}
	return Profile{}, false
}
