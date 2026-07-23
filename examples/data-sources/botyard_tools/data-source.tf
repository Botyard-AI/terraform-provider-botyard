# List the org tool catalog and select the ids of every GitHub-domain tool.
data "botyard_tools" "all" {}

locals {
  github_tool_ids = [
    for t in data.botyard_tools.all.tools : t.id if t.domain == "github"
  ]
}
