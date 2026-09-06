package pkrkit

type Hashes struct {
	SHA256 string
	SHA1   string
	MD5    string
	SHA512 string
}

type Descriptor struct {
	Digest    string
	MediaType string
	Size      int64
	Name      string
}

func (d Descriptor) Hex() string {
	if s, ok := hexOfDigest(d.Digest); ok {
		return s
	}
	return d.Digest
}

func hexOfDigest(d string) (string, bool) {
	for i := 0; i < len(d); i++ {
		if d[i] == ':' {
			return d[i+1:], true
		}
	}
	return "", false
}

func (d Descriptor) IsEmpty() bool { return d.Digest == "" }

type Artifact struct {
	Format      string
	Repository  string
	Version     string
	MediaType   string
	Proprietary []byte
	Digest      string
	Blobs       []Descriptor
	Source      string
}

type PackageSummary struct {
	Format     string `json:"format"`
	Repository string `json:"repository"`
	Version    string `json:"version"`
	MediaType  string `json:"media_type,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Size       int64  `json:"size"`
}
