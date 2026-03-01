package client

import "fmt"

// BaseURLs maps module names to their base URLs for a given environment.
type BaseURLs struct {
	BorrowerCentral string
	CMS             string
	AltData         string
	Auth            string
}

var envURLs = map[string]BaseURLs{
	"production": {
		BorrowerCentral: "https://bc.altscore.ai",
		CMS:             "https://api.altscore.ai",
		AltData:         "https://data.altscore.ai",
		Auth:            "https://auth.altscore.ai",
	},
	"staging": {
		BorrowerCentral: "https://borrower-central-staging-zosvdgvuuq-uc.a.run.app",
		CMS:             "https://api.stg.altscore.ai",
		AltData:         "",
		Auth:            "https://altscore-stg.us.frontegg.com",
	},
	"sandbox": {
		BorrowerCentral: "https://bc.sandbox.altscore.ai",
		CMS:             "https://api.sandbox.altscore.ai",
		AltData:         "",
		Auth:            "https://auth.sandbox.altscore.ai",
	},
}

// GetBaseURLs returns the base URLs for the given environment.
func GetBaseURLs(environment string) (BaseURLs, error) {
	urls, ok := envURLs[environment]
	if !ok {
		return BaseURLs{}, fmt.Errorf("unknown environment %q (valid: production, staging, sandbox)", environment)
	}
	return urls, nil
}

// ModuleURL returns the base URL for a specific module in the given environment.
func ModuleURL(environment, module string) (string, error) {
	urls, err := GetBaseURLs(environment)
	if err != nil {
		return "", err
	}
	switch module {
	case "borrower_central":
		return urls.BorrowerCentral, nil
	case "cms":
		return urls.CMS, nil
	case "altdata":
		if urls.AltData == "" {
			return "", fmt.Errorf("altdata module is not available in the %q environment", environment)
		}
		return urls.AltData, nil
	case "auth":
		return urls.Auth, nil
	default:
		return "", fmt.Errorf("unknown module %q", module)
	}
}
