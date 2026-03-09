package platforms

type PlatformCapabilities struct {
	SupportsLongForm  bool
	SupportsShortForm bool
	SupportsMarkdown  bool
	SupportsThreads   bool
	SupportsEdit      bool
	SupportsDelete    bool
}

func Capabilities(platform string) PlatformCapabilities {
	switch platform {
	case "devto":
		return PlatformCapabilities{SupportsLongForm: true, SupportsMarkdown: true}
	case "hashnode":
		return PlatformCapabilities{SupportsLongForm: true, SupportsMarkdown: true, SupportsEdit: true, SupportsDelete: true}
	case "bluesky":
		return PlatformCapabilities{SupportsShortForm: true, SupportsThreads: true}
	case "x":
		return PlatformCapabilities{SupportsShortForm: true}
	case "reddit":
		return PlatformCapabilities{SupportsLongForm: true, SupportsShortForm: true, SupportsMarkdown: true}
	case "medium":
		return PlatformCapabilities{SupportsLongForm: true, SupportsMarkdown: true, SupportsEdit: true}
	case "substack":
		return PlatformCapabilities{SupportsLongForm: true, SupportsMarkdown: true}
	default:
		return PlatformCapabilities{}
	}
}
