# Data sales

A seller and a buyer that do not trust each other trade one dataset. The
seller delivers it encrypted over a Room that holds only the two of them; the
buyer checks the delivery against the seller's signed commitments and pays the
price into an escrow on an EVM chain; the seller reveals the key; the buyer
decrypts and checks every 4 KiB leaf. If a leaf does not match, the buyer's
challenge refunds it. If the seller never reveals the key, the buyer reclaims
the payment once the reveal period ends. Otherwise the seller settles once the
challenge window closes. An honest sale takes three transactions: fund, reveal
and settle.

```text
beam sale wallet create [--import-stdin]
beam sale wallet show
beam sale wallet backup
beam sale wallet limits [--per-sale <tao>] [--daily <tao>] [--approve-above <tao>] [--delivery-budget <usd>]
beam sale wallet send --to <address> (--amount <tao> | --all)
beam sale wallet remove

beam sale room create [--api-key <key>] [--max-size <size>] [--invitation-ttl <duration>] [--out <file>]
beam sale offer <room> --file <dataset> --price <tao> --binding <file>
    [--reveal-period <blocks>] [--challenge-period <blocks>] [--fund-within <blocks>]
    [--tao-usd <price>] [--package-dir <dir>] [--name <name>]
beam sale revoke <sale>

beam sale join <room> (--invitation-file <file> | --invitation-token <token>) [--out <binding-file>]
beam sale buy <room> --out <file> --max-price <tao> [--sale <sale>] [--wait <duration>]

beam sale status [<sale>]
beam sale resume <sale> [--wait]
beam sale withdraw
```

A release build uses Beam's DataSaleEscrow v1 deployment on Bittensor EVM
mainnet by default, and a development or pull-request build its testnet
deployment (see [Configuration](#configuration)). A context can select another
with `sale_chain_id`, `sale_rpc_urls` (comma-separated, in order of preference;
reads go to the first that answers and every transaction is broadcast to all of
them) and `sale_escrow`. A sale
checks the chain once a block. An endpoint that refuses a call for too many
requests (HTTP 429) gets no call for as long as it asks, one minute by default,
so its limit can clear. When every endpoint is waiting, the command waits too
and says so, up to five times for one call. Public endpoints limit each
address, so an agent that runs many sales should list an endpoint of its own
first. A sale id
may be shortened to any unique prefix.

A sale runs in this order:

1. The seller runs `sale room create`, which starts a billable Room with an
   object channel and writes a one-use invitation, granting only discovery and
   delivery on that channel, to `--out`.
2. The buyer runs `sale join`, which joins the Room and signs a binding: a
   statement, signed by the buyer's sale wallet, that its member in this Room
   buys for this wallet, on this chain and escrow. The buyer gives the binding
   to the seller.
3. The seller runs `sale offer`. It checks the binding, and checks that the
   Room holds only the seller and the bound buyer, every membership counted. It
   writes the encrypted package (about 101% of the dataset) to `--package-dir`,
   delivers it to the buyer's member only, and waits for funding until the
   offer's deadline. A buyer that joined moments before may not hold the
   channel's key yet; that delivery fails at once, and the seller publishes
   the package again, up to three times. That deadline falls `--fund-within` blocks after the end
   of the delivery window: 30 minutes plus 1 second per 4 MiB. Once the
   funding is in a finalized block, it reveals the key, then exits with the
   block after which the sale can be settled.
4. The buyer runs `sale buy`. It waits up to `--wait` (default 30m) for the
   package, reads it from its agent's inbox, and checks it before funding:
   - the signature;
   - that the offer names this wallet, chain and escrow, and the package's file
     name names the offer's sale;
   - the ciphertext root and the hash-list root;
   - the price against `--max-price`, and the periods.

   It then funds and waits for the key. It writes the decrypted dataset to
   `--out` only if every leaf matches; otherwise it keeps it as
   `<out>.unverified`, records the first mismatching leaf with its committed
   hash, and challenges it. The challenge needs nothing more from the inbox.
5. The seller runs `sale resume <sale>` after the challenge window to settle.
   `resume --wait` stays until nothing is left to do.

`sale resume` carries any sale forward from its escrow state. It reveals,
settles, reclaims an unrevealed payment, decrypts, or challenges, whichever is
due, and resumes any transaction an interrupted run broadcast rather than
sending a new one. `sale status` lists the sales on this machine with each
one's escrow state, next step and the block it is due by.

Rules the client enforces:

- **Price:** at most 1 TAO, the escrow's immutable cap, and at least $10 at a
  TAO/USD quote fetched from CoinGecko or given with `--tao-usd`. The request
  carries no data about the sale.
- **Size:** at most 100 GB.
- **Default periods:** 1,800 blocks to reveal (about 6 hours) and 7,200 blocks
  to challenge (about 24 hours). Neither may be below the escrow's floor.
- **Reveal:** sent only once the funding is in a finalized block, checked
  locally against everything the escrow checks, never simulated through an RPC
  endpoint, and never first broadcast within 100 blocks of its deadline. Once
  a reveal was broadcast the key is public, so it is sent again up to the
  deadline itself. A stalled transaction has its fee raised, up to
  1,000 gwei. When the main account is stuck, a backup account
  (`sale wallet backup`) sends reveals, refunds, settlements and challenges.
  A sale step that finds the account busy with a transaction this client did
  not send waits for it, as it waits for an RPC endpoint that does not answer.
- **Seller limits:**
  - one unfunded delivery per buyer address at a time;
  - delivery only to a buyer that holds the price plus gas;
  - a daily budget, $5 by default, for the Beam charges of deliveries to
    buyers who have not funded. The client estimates those charges at 0.2
    credit plus 0.02 credit per GB, and 1 credit for the Room's first delivery.
- **Buyer limits:** 1 TAO per sale and 3 TAO a day by default. A person must
  confirm any sale above 0.2 TAO.
- **Person-only actions:** approving a spend above the threshold, raising a
  limit, moving TAO out with `sale wallet send`, and removing the wallet all
  need a person at an interactive terminal, and no flag stands in for that.
  This holds for an agent that runs `beam` commands. It does not hold against
  software that runs as the wallet's OS account with wider access: that
  software can read the keys, edit the limits, or drive a pseudo-terminal.
  Keep only what you can afford to lose in the sale wallet, and refill it from
  a wallet a person holds.
- **Keys:** the wallet's keys and secret live in the OS credential store, or an
  owner-only `sale-wallet.json` where there is none. They are never printed.
  Each sale's key is derived from the wallet secret and the sale id.

Before a command uses the escrow, the client reads the code at `sale_escrow`
and refuses it (`sale_escrow_unverified`, exit code 3) unless it is the
`DataSaleEscrow` v1 build: the same runtime code byte for byte, except the
immutables each deployment writes (its price cap, its period floors, and the
EIP-712 domain, which includes its address and chain id). So `sale_escrow` can
point only at a deployment of that contract. `sale join`, `offer`, `buy`,
`resume`, `revoke` and `withdraw` refuse; `status` and `wallet show` leave out
what they would read from the escrow; `wallet send` works whatever the address
holds, so a wrong address never strands the wallet's balance.

What the client trusts:

- **The RPC endpoints.** The client believes what `sale_rpc_urls` report about
  the chain, including the code at the escrow address. Use endpoints you
  trust.
- **The buyer agent's inbox.** The buyer reads the delivered package from its
  agent until the key is revealed and the package decrypted. An agent run with
  `--room-inbox-max-age` or `--room-inbox-max-gib` removes delivered objects;
  set either well above the reveal period, or the buyer cannot decrypt or
  challenge what it paid for.

The escrow guarantees that the delivered bytes match the seller's commitments,
not that the data is what the buyer wanted. Once revealed, the key is public:
anyone holding the encrypted package can then read the dataset, which is why
the package goes only to the buyer's member in a two-member Room.

Sale records live under `sales/` in the Beam configuration directory. The
seller removes its package once it is delivered. The buyer keeps its proof
trees (about 3% of the dataset) and the delivered package in its agent's inbox
only while a challenge could still need them: both are removed once every leaf
matches, or when the sale ends.

## Configuration

| Config key | Variable | Meaning |
| --- | --- | --- |
| `sale_chain_id` | `BEAM_SALE_CHAIN_ID` | chain id of the data-sale escrow |
| `sale_rpc_urls` | `BEAM_SALE_RPC_URLS` | comma-separated JSON-RPC endpoints for that chain, in order of preference |
| `sale_escrow` | `BEAM_SALE_ESCROW` | address of the data-sale escrow |

Each build carries its defaults, injected at build time like the service URLs,
and `beam config unset` restores them:

| Build | Chain | Endpoints | Escrow |
| --- | --- | --- | --- |
| Release (`beam`) | Bittensor EVM mainnet, `964` | `https://lite.chain.opentensor.ai`, `https://entrypoint-finney.opentensor.ai` | `0xe26A702B55B436615694624EF60b856C9d9f0674` |
| Development and pull request | Bittensor EVM testnet, `945` | `https://test.chain.opentensor.ai` | `0xA689e83852bde998FAdd71C96acc16724E67628b` |

## The agent's inbox

`beam sale buy` reads a delivered sale package through the inbox routes of the
[local API](daemon-local-api.md) when `/v1/status` advertises `btr.inbox.list`
and `btr.inbox.read`. It checks and decrypts a large package with ranged reads,
without copying it. Once the package is no longer needed to prove a leaf, the
buyer deletes it when `/v1/status` advertises `btr.inbox.delete`. A seller
publishes the package with the existing object routes, after checking
`btr.object.publish`.

## Architecture

```text
beam -- JSON-RPC --> EVM chain: data-sale escrow
     -- private local API --> tunnel agent: sale Room, package delivery, inbox
```

- `internal/datasale`: the data-sale protocol the escrow checks: leaf hashes,
  Merkle trees on disk, encryption, the EIP-712 offer and the per-sale key.
  Shared test vectors pin it to the contract byte for byte.
- `internal/escrow`: the escrow's ABI, an RPC client that fails over between
  endpoints, and a sender that gets a transaction included before its deadline.
- `internal/salewallet`: the sale wallet's keys, its spending limits, and the
  ledger that enforces them across processes.
- `internal/saleroom`: carrying a sale over a Room: the buyer's binding, the
  two-member rule, and the sale package's format, builder and checks.
- `internal/salestate`: the per-sale records that let a sale resume after a
  restart.
- `internal/command/sale*.go`: the `beam sale` commands.

The data-sale wallet's keys use the OS credential store or an owner-only file,
are never printed, and never reach the agent. A sale key is derived from the
wallet secret and broadcast only in the reveal transaction, once the funding is
in a finalized block and every condition that could make it revert was checked
locally; it is never simulated through an RPC endpoint.

The client uses `github.com/ethereum/go-ethereum` for the escrow's ABI, signing
and JSON-RPC.

## The agent skill

The agent reference for sales is
[skills/beam/references/data-sales.md](../skills/beam/references/data-sales.md),
routed from `skills/beam/SKILL.md`.
