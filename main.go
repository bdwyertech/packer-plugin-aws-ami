package main

import (
	"fmt"
	"os"

	"github.com/hashicorp/packer-plugin-sdk/plugin"

	"github.com/bdwyertech/packer-plugin-ami-copy/version"
)

func main() {
	pps := plugin.NewSet()
	pps.RegisterPostProcessor(plugin.DEFAULT_NAME, new(PostProcessor))
	pps.SetVersion(version.PluginVersion)
	err := pps.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
