package schema

// Track represents a single audio file in our catalog
//type Track struct {
//	ID       string `json:"id"`
//	Title    string `json:"title"`
//	Artist   string `json:"artist"`
//	FileName string `json:"fileName"`
//}

// Catalog is the wrapper for our versioned manifest
//type Catalog struct {
//	Version string  `json:"version"`
//	Count   int     `json:"count"`
//	Tracks  []Track `json:"tracks"`
//}

// CatalogSchema defines the exact fields we want to extract from any data source (like a CSV).
// If you add a new field to the system, add its JSON key name here.
var CatalogSchema = []string{
	"filename",
	"title",
	"artist",
	"hash",
	"speaker",
	"category",
	"keywords",
	"year",
	"topten",
	"audio-hash",
}
