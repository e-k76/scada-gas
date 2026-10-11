// Package deploy embeds the SQL scripts the services apply themselves.
package deploy

import _ "embed"

// TopologySQL is deploy/topology_init.sql.
//
//go:embed topology_init.sql
var TopologySQL string
