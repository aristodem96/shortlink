package bot

// Every string a user reads lives in this file, so the bot can be translated
// or re-worded without touching any logic. The copy is Ukrainian because that
// is the audience; the code around it is English like the rest of the project.
//
// All of it is sent with parse_mode=HTML. Anything interpolated that came from
// a user -- a key name -- must go through html.EscapeString first, or a name
// such as "<b>" would either be rendered or make Telegram reject the message.

const (
	msgPrivateOnly = "Я працюю лише в особистих повідомленнях — тут видаються секретні ключі, їм не місце в групі."

	msgUnknownCommand = "Не знаю такої команди. Спробуйте /help."

	msgInternalError = "Щось пішло не так на нашому боці. Спробуйте ще раз за хвилину."

	msgNoAccess = "У вас немає активного доступу.\n\nОплатіть його командою /buy — після оплати зможете створювати ключі тут же."

	msgNoKeys = "У вас поки немає ключів. Створіть перший: /newkey"

	msgRevokeUsage = "Вкажіть номер ключа, наприклад: <code>/revoke 12</code>\nНомери показує /keys."

	msgKeyNotFound = "Ключа з таким номером серед ваших немає. Перевірте /keys."

	msgKeyRevoked = "Ключ відкликано. Він більше не працює."

	msgPayButton = "💳 Оплатити"

	msgDocsButton = "📖 Документація API"
)

// Templates filled with fmt.Sprintf. The %s/%d order is part of each one's
// contract, so the call sites in bot.go pass arguments in the order noted.

// tplStart: price, days, days, max keys.
const tplStart = `Привіт! Це магазин доступу до <b>shortlink API</b> — сервісу коротких посилань зі статистикою переходів.

<b>Як це працює</b>
1. /buy — оплата <b>$%s</b> криптовалютою через @CryptoBot
2. Отримуєте доступ на <b>%d днів</b>
3. /newkey — створюєте собі API-ключі (до %d штук), вони працюють усі %d днів

Команди — /help`

// tplHelp: no arguments.
const tplHelp = `<b>Команди</b>

/buy — оплатити доступ
/status — скільки доступу лишилось
/newkey <i>назва</i> — створити API-ключ
/keys — ваші ключі
/revoke <i>номер</i> — відкликати ключ
/help — ця довідка

Повторна оплата додає дні до поточного терміну, а не згоряє — і продовжує всі ваші ключі.`

// tplCheckout: price, days.
const tplCheckout = `Доступ до API на <b>%[2]d днів</b> — <b>$%[1]s</b>.

Натисніть кнопку, щоб оплатити в @CryptoBot. Після оплати я напишу сам — зазвичай за кілька секунд.`

// tplPaid: expiry date.
const tplPaid = `✅ Оплату отримано!

Доступ активний до <b>%s</b>.

Створіть ключ командою /newkey`

// tplStatusActive: expiry date, key count, max keys.
const tplStatusActive = `Доступ активний до <b>%s</b>.
Ключів: %d з %d.`

// tplStatusExpired: expiry date.
const tplStatusExpired = `Доступ закінчився <b>%s</b>. Ключі не працюють, але збережені — оплата через /buy поверне їх до життя.`

// tplKeyIssued: name, secret, expiry date, api base url.
const tplKeyIssued = `🔑 Ключ <b>%s</b> створено:

<code>%s</code>

Збережіть його зараз — я зберігаю лише хеш і показати ключ ще раз не зможу.

Діє до <b>%s</b>. Передавайте в заголовку:
<code>Authorization: Bearer ...</code>

API: %s`

// tplKeyLimit: max keys.
const tplKeyLimit = `У вас уже %d ключів — це максимум. Відкличте непотрібний через /revoke і створіть новий.`

// tplKeysHeader: key count, max keys.
const tplKeysHeader = "<b>Ваші ключі</b> (%d з %d):\n"

// tplKeyLine: id, name, expiry date.
const tplKeyLine = "\n<code>%d</code> · %s · до %s"

const tplKeysFooter = "\n\nВідкликати: <code>/revoke номер</code>"

// tplBadName: reason.
const tplBadName = "Назва ключа не підходить: %s."
