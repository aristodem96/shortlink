package bot

// Every string a user reads lives in this file, so the bot can be reworded or
// translated without touching any logic.
//
// All of it is sent with parse_mode=HTML. Anything interpolated that did not
// come from this file -- a key name, a target URL, a referrer, which arrives in
// a request header anybody can set -- must go through html.EscapeString first,
// or a value such as "<b>" would either be rendered or make Telegram reject the
// message.

const (
	msgPrivateOnly = "I only work in private messages — I hand out secret keys, and they don't belong in a group."

	msgUnknownCommand = "I don't know that command. Try /help."

	msgInternalError = "Something went wrong on our side. Please try again in a minute."

	msgNoAccess = "You don't have active access.\n\nPay for it with /buy — after that you can create keys and shorten links right here."

	msgNoKey = "You don't have a key yet. Create one with /newkey — the bot uses it for the links you send here."

	msgNoKeys = "You don't have any keys yet. Create your first one: /newkey"

	msgRevokeUsage = "Give the key number, for example: <code>/revoke 12</code>\n/keys shows the numbers."

	msgUseKeyUsage = "Give a key number from /keys, or paste the key itself, for example: <code>/usekey 12</code>"

	msgKeyNotFound = "None of your live keys has that number. Check /keys."

	msgKeyRevoked = "Key revoked. It no longer works."

	msgShortenUsage = "Send me a link to shorten, optionally followed by an alias:\n<code>https://example.com/very/long/page</code>\n<code>https://example.com my-alias</code>"

	msgAliasTaken = "That alias is already taken. Try another one, or send the link without an alias to get a random code."

	msgNoLinks = "No links yet. Send me any URL and I'll shorten it."

	msgStatsUsage = "Give the code of one of your links, for example: <code>/stats abc1234</code>\n/links shows your codes."

	msgLinkNotFound = "There's no link with that code among the bot key's links. Check /links."

	msgPayButton = "💳 Pay"

	msgDocsButton = "📖 API docs"

	msgOpenButton = "↗ Open"
)

// Templates filled with fmt.Sprintf. The argument order noted on each one is
// part of its contract with the call sites in bot.go.

// tplStart: price, days, max keys.
const tplStart = `Hi! This bot sells access to the <b>shortlink API</b> — short links with click analytics.

<b>How it works</b>
1. /buy — pay <b>$%s</b> in crypto via @CryptoBot
2. You get <b>%d days</b> of access
3. /newkey — create an API key (up to %d)
4. Send me any link and I'll shorten it — /links shows your links with their clicks

All commands — /help`

// tplHelp: no arguments.
const tplHelp = `<b>Links</b>
Send any URL — I'll shorten it
<code>https://example.com my-alias</code> — with a custom alias
/links — your recent links with clicks
/stats <i>code</i> — detailed clicks for one link

<b>Access and keys</b>
/buy — pay for access
/status — how much access is left
/newkey <i>name</i> — create an API key
/keys — your keys
/usekey <i>number</i> — choose the key the bot uses for links
/revoke <i>number</i> — revoke a key

Paying again adds days on top of the time you have left and extends all of your keys.`

// tplCheckout: price, days.
const tplCheckout = `API access for <b>%[2]d days</b> — <b>$%[1]s</b>.

Tap the button to pay in @CryptoBot. I'll message you as soon as the payment arrives — usually within seconds.`

// tplPaid: expiry date.
const tplPaid = `✅ Payment received!

Your access is active until <b>%s</b>.

Create a key with /newkey, then send me a link to shorten.`

// tplStatusActive: expiry date, key count, max keys, bot key line.
const tplStatusActive = `Access active until <b>%s</b>.
Keys: %d of %d.
%s`

// tplStatusExpired: expiry date.
const tplStatusExpired = `Your access ended on <b>%s</b>. Your keys and links are kept but don't work — paying with /buy brings them back.`

// tplBotKeyLine: key id, key name.
const tplBotKeyLine = "Bot key: <code>%d</code> · %s"

const msgBotKeyNone = "Bot key: none — create one with /newkey"

// tplKeyIssued: name, secret, expiry date, api base url, activation note.
const tplKeyIssued = `🔑 Key <b>%s</b> created:

<code>%s</code>

Save it now — I only keep its hash and can't show it again.

Valid until <b>%s</b>. Send it in the header
<code>Authorization: Bearer ...</code>

API: %s

%s`

const msgKeyActivated = "The bot will use this key for the links you send here."

// tplKeyNotActivated: current bot key id.
const tplKeyNotActivated = "The bot keeps using key <code>%d</code> for links. Switch with <code>/usekey</code>."

// tplKeyLimit: max keys.
const tplKeyLimit = `You already have %d keys — that's the maximum. Revoke one you don't need with /revoke and create a new one.`

// tplKeysHeader: key count, max keys.
const tplKeysHeader = "<b>Your keys</b> (%d of %d):\n"

// tplKeyLine: id, name, expiry date, bot marker.
const tplKeyLine = "\n<code>%d</code> · %s · until %s%s"

const keyBotMarker = " · ★ bot"

const tplKeysFooter = "\n\nRevoke: <code>/revoke number</code>\nUse for links: <code>/usekey number</code>"

// tplKeySelected: id, name.
const tplKeySelected = "✅ The bot now uses key <code>%d</code> · %s for new links."

// tplBadName: reason.
const tplBadName = "That key name won't work: %s."

// tplShortened: short url, target url.
const tplShortened = `🔗 <b>%s</b>

→ %s`

// tplCannotShorten: reason.
const tplCannotShorten = "I can't shorten that: %s."

// tplLinksHeader: shown count, total count.
const tplLinksHeader = "<b>Your links</b> (latest %d of %d):\n"

// tplLinkLine: code, clicks, target.
const tplLinkLine = "\n<code>%s</code> · %s\n→ %s\n"

const tplLinksFooter = "\nDetails: <code>/stats code</code>"

// tplStats: code, total, unique, last click.
const tplStats = `📊 <b>%s</b> — last 30 days

Clicks: <b>%d</b>
Unique visitors: <b>%d</b>
Last click: %s`

const tplStatsReferrersHeader = "\n\n<b>Top referrers</b>"

// tplStatsLine: label, count.
const tplStatsLine = "\n%s — %d"

const tplStatsDaysHeader = "\n\n<b>By day</b>"

const msgNever = "never"
