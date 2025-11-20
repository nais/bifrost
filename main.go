package main

import "github.com/nais/bifrost/cmd"

//	@title			Bifrost API
//	@version		1.0
//	@description	API for managing Unleash (feature flag) instances
//	@description	This service provides a JSON API for creating, updating, and deleting Unleash instances.

//	@contact.name	NAIS Team
//	@contact.url	https://github.com/nais/bifrost

//	@license.name	MIT
//	@license.url	https://github.com/nais/bifrost/blob/main/LICENSE

//	@host		localhost:8080
//	@BasePath	/

func main() {
	_ = cmd.Execute()
}
