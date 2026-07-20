// terraform-provider-botyard is the Terraform provider for the Botyard
// platform. It manages Botyard resources (bots, skills, workforces,
// credentials, ...) declaratively through the public Botyard API.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/provider"
)

// version is set by the release build (GoReleaser) via -ldflags; "dev" locally.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		// Matches the public Terraform Registry address for the provider.
		Address: "registry.terraform.io/Botyard-AI/botyard",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
