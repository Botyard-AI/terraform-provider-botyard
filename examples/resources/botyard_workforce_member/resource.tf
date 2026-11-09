# Manage one member, not the board or its complete member list.
# The API must support expected_workforce_id and the identity acknowledgement.
variable "workforce_id" {
  type        = string
  description = "Immutable UUID of an existing workforce board, not its mutable slug."
}

variable "bot_id" {
  type        = string
  description = "UUID of the existing bot to add."
}

resource "botyard_workforce_member" "reviewer" {
  workforce_id = var.workforce_id
  actor_type   = "bot"
  actor_id     = var.bot_id
  role         = "member"
}

# Import existing rows to adopt their current role before changing it.
# Removing this resource removes only this member row. To leave access intact,
# relinquish management with terraform state rm instead.
