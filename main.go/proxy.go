type TLSConfig struct {
	CertFile   string `json:"cert_file"`
	KeyFile    string `json:"key_file"`
	MinVersion string `json:"min_version"` // "1.2" (default) or "1.3"

	CertPEM []byte `json:"-"`
	KeyPEM  []byte `json:"-"`
