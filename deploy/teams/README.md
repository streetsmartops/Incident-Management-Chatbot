# Teams app (optional)

1. Register an Azure Bot (multi-tenant, or single-tenant and set `teams.tenantId`).
   Messaging endpoint: `https://<host>/teams/messages`.
2. Put the bot's App ID in `teams.appId` (config) and in `manifest.json` (`id`, `bots[0].botId`).
   The client secret goes in the `TEAMS_APP_PASSWORD` secret.
3. Add `color.png` (192x192) and `outline.png` (32x32), zip with `manifest.json`, upload to Teams.
4. Install the app in the incident announce channel. Set `teams.announceConversationId`
   and `teams.announceServiceUrl` (take both from a message activity the bot receives there).

Limits: bots cannot create channels without Microsoft Graph, so Teams incidents run in the
announce channel; use `--incident INC-YYYY-NNNN` with commands.
