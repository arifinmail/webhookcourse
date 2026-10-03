# WhatsApp orders

Reads the WhatsApp messages your customers send you, in private chats and in the
groups you choose. Claude turns them into orders, and each order goes to your own
system as a webhook.

```
WhatsApp ─► filter ─► wait until the ─► Claude fills in ─► POST to your system
(linked     (your     customer stops    the order          (signed JSON,
 device)     rules)    typing)                               retried if it's down)
```

## Good to know first

- It links to your WhatsApp as a linked device, like WhatsApp Web, using
  [whatsmeow](https://github.com/tulir/whatsmeow), an unofficial library. WhatsApp
  doesn't allow unofficial apps, so there is a risk of your number being banned. To
  keep that risk low, this program never sends anything: no messages, no read
  receipts, no "online" status.
- Only messages that pass your filter go to Claude. Everything else stays on your computer.
- Claude only fills in an order form. It can't send messages or see other chats, and
  it ignores instructions written inside messages, so a trick message can at worst
  produce a wrong order.
- It has to keep running to see new messages: use a computer that stays on, or a small
  server. Messages that arrive while it's off are picked up when it starts again
  (up to `filter.max_age` old). When Claude can't be reached (no internet, a wrong API
  key), messages wait and are tried again, so no order gets lost.
- WhatsApp logs linked devices out when your phone hasn't been online for 14 days.
- The `store` folder holds your WhatsApp login and your customers' messages. Keep it
  private and never upload it.

## Set up

1. Install Go 1.26 or newer from <https://go.dev/dl/>.
2. Create a Claude API key at <https://platform.claude.com/> and set it in the
   terminal you'll use:
   - macOS / Linux: `export ANTHROPIC_API_KEY=sk-ant-...`
   - Windows (PowerShell): `$env:ANTHROPIC_API_KEY="sk-ant-..."`
3. In this folder, copy `config.example.yaml` to `config.yaml` and fill in your
   products, groups and filter.
4. Check that Claude reads your orders correctly before connecting WhatsApp. Each line
   is one message:
   ```
   go run . -try "Kak mau pesan 2 dimsum ayam ya, kirim ke Jl. Melati 5 besok sore"
   ```
5. Start it and link WhatsApp:
   ```
   go run .
   ```
   On your phone open WhatsApp > Linked devices > Link a device and scan the QR code.
   If scanning is awkward, link with a code instead:
   `go run . -pair-phone 6281234567890` (your own number).
   From then on, `go run .` is all you need.

While `output.webhook_url` is empty the program runs in test mode: orders only go to
`store/orders.jsonl`, one per line. That's a good way to watch it for a few days before
connecting your system. Orders made in test mode are not sent later. Add `-v` to see why
messages are skipped (their text is never logged).

## Receiving orders in your system

Every order is sent as a `POST` to `output.webhook_url`:

```json
{
  "id": "ord_20261003T071502_9a353a9e",
  "created_at": "2026-10-03T07:15:02Z",
  "source": {
    "chat_type": "group",
    "chat_id": "120363000000000001@g.us",
    "chat_name": "PO Frozen Food Oktober",
    "sender_phone": "6281234567890",
    "sender_name": "Budi"
  },
  "messages": [
    { "id": "3EB0A1", "time": "2026-10-03T07:12:40Z", "text": "Kak mau pesan" },
    { "id": "3EB0A2", "time": "2026-10-03T07:13:05Z", "text": "2 dimsum ayam, kirim ke Jl. Melati 5 besok sore" }
  ],
  "order": {
    "is_order": true,
    "customer_name": "Budi",
    "items": [{ "product": "Dimsum ayam (isi 10)", "quantity": 2, "unit": "", "notes": "" }],
    "delivery_address": "Jl. Melati 5",
    "delivery_time": "besok sore (Sun 2026-10-04)",
    "payment_method": "",
    "notes": "",
    "missing_info": ["metode pembayaran"],
    "needs_review": false,
    "summary": "2 dimsum ayam, kirim ke Jl. Melati 5 besok sore"
  },
  "model": "claude-opus-5-5"
}
```

- `X-Signature-256: sha256=<hex>` is the HMAC-SHA256 of the raw body with your
  secret. Reject requests where it doesn't match.
- Answer with any 2xx status once the order is saved. Otherwise it's sent again after
  1, 2, 4, ... minutes, then every hour, until your system accepts it.
- The same order can arrive twice (for example when your system saved it but answered
  too slowly). `id` is also in the `X-Order-Id` header: skip ids you already have.
- `needs_review: true` means something was unclear; `missing_info` says what to ask.
- A customer who adds to an order after `quiet_period` sends a second order. Use
  `source.sender_phone` and `source.chat_id` to put them together.

A receiver in Node.js with Express, like `code/express-discorder`:

```js
const crypto = require("crypto");
const express = require("express");

const app = express();

app.post("/orders", express.raw({ type: "application/json" }), (req, res) => {
  const expected = "sha256=" + crypto
    .createHmac("sha256", process.env.ORDER_WEBHOOK_SECRET)
    .update(req.body)
    .digest("hex");
  const given = req.get("X-Signature-256") || "";
  if (given.length !== expected.length ||
      !crypto.timingSafeEqual(Buffer.from(given), Buffer.from(expected))) {
    return res.status(401).send("Bad signature");
  }
  const event = JSON.parse(req.body);
  // Save event.order in your system here, and skip event.id if you already have it.
  console.log(`Order ${event.id} from ${event.source.sender_name}: ${event.order.summary}`);
  res.sendStatus(200);
});

app.listen(3000, () => console.log("Waiting for orders on port 3000"));
```

Use the same secret on both sides: `ORDER_WEBHOOK_SECRET` here, and `output.secret`
(or the same environment variable) for this program.

## What reaches Claude

- Never: your own messages, chats from `ignore_numbers`, groups not in `groups`,
  status updates and channels.
- Only text: plain messages and the captions of photos, videos and documents.
  Stickers, voice notes and photos without a caption are skipped.
- A customer's messages are collected until they have been quiet for `quiet_period`,
  then read together as one order.

## Cost

Claude Opus 5.5 costs $4 per million input tokens and $20 per million output tokens,
which comes to roughly 1-2 US cents per batch of messages it reads. Setting
`claude.effort: low` makes it cheaper. You can also put another Claude model in
`claude.model` (set `claude.effort: ""` if that model doesn't support effort); check it
with `-try` on some real messages first.

## Files

| File | What it does |
| --- | --- |
| `whatsapp.go` | Linked-device connection; turns WhatsApp messages into plain text with sender and chat |
| `filter.go` | Your filter rules |
| `batcher.go` | Waits until a customer stops typing |
| `extract.go` | Asks Claude to fill in the order (structured output, so the answer always parses) |
| `deliver.go` | Signs and sends orders, retries when your system is down |
| `store.go` | SQLite file with messages and orders, so a restart loses nothing |
| `pipeline.go` | Connects the steps |

Run the tests with `go test ./...`.
