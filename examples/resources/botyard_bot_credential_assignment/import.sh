# Import ownership of every scope the bot currently has credential assignments in:
terraform import botyard_bot_credential_assignment.example my-bot-slug

# Or import ownership of only specific scopes (recommended when Terraform should
# manage a subset of the bot's credential scopes):
terraform import botyard_bot_credential_assignment.example 'my-bot-slug:llm,web_search'
