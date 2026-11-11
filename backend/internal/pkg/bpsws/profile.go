package bpsws

import "runtime"

func excelProfile() map[string]string {
	platform := "PC"
	if runtime.GOOS == "darwin" {
		platform = "Mac"
	}
	return map[string]string{
		"X-Openai-Internal-Basispoints-Client-Product":        "basispoints-excel-plugin",
		"X-Openai-Internal-Basispoints-Client-Platform":       "excel",
		"X-Openai-Internal-Basispoints-Client-Agent-Profile":  "excel",
		"X-Openai-Internal-Basispoints-Client-Editor":         "excel",
		"X-Openai-Internal-Basispoints-Client-Host":           "office",
		"X-Openai-Internal-Basispoints-Client-Runtime":        "desktop",
		"X-Openai-Internal-Basispoints-Client-Platform-Class": platform,
		"X-Openai-Internal-Basispoints-Office-Host":           "Excel",
		"X-Openai-Internal-Basispoints-Office-Platform":       platform,
		"X-Openai-Internal-Basispoints-Browser-Name":          "unknown",
	}
}
