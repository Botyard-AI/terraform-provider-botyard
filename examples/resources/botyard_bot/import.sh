# Bots are imported by their slug (the API's URL identifier).
terraform import botyard_bot.support support-assistant

# A bot with a Bot Pack install: `<slug>` alone adopts the install only when the
# bot has exactly one (none imports no `pack` block). With several installs it
# is an error that lists them; name the one this resource manages instead, by
# install id or, when that pack slug is installed once on the bot, by pack slug:
terraform import botyard_bot.support support-assistant:0b6c1f9e-4f7a-4d2b-9a51-3c2e8d7f6a10
terraform import botyard_bot.support support-assistant:crm-assistant
# `pack.settings` and `pack.connections` are create-time inputs the API cannot
# read back, so they are not imported.
