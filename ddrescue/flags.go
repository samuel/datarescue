package ddrescue

import "flag"

// Usage strings for the flags below. They are exported so a tool that appends
// its own caveat to one still uses the same wording for the rest.
const (
	MapUsage   = "ddrescue .map for the image, which otherwise uses input.img.map or input.map when one is there"
	NoMapUsage = "ignore the <image>.map sidecar and read the image as it is"
)

// Flags are the source flags a tool registers. They mirror outsink.Flags:
// outsink.Flags sets where recovered files go, and Flags sets where the bytes
// come from and what is known about their damage.
type Flags struct {
	Map   string
	NoMap bool
}

// Register adds the flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.Map, "map", "", MapUsage)
	fs.BoolVar(&f.NoMap, "no-map", false, NoMapUsage)
}

// Options is the flags as the policy takes them.
func (f *Flags) Options() Options { return Options{Map: f.Map, NoMap: f.NoMap} }

// Validate rejects -map given together with -no-map, which has no meaning, and
// checks that a given map exists. A tool can then reject a bad command line
// before it touches the image, instead of letting the operator believe a flag
// took effect.
func (f *Flags) Validate() error {
	_, err := Resolve("", f.Options())
	return err
}

// Open resolves the map and opens the image under it.
func (f *Flags) Open(path string) (*Source, error) {
	return OpenSource(path, f.Options())
}
