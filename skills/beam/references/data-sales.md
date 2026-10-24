# Beam data sales reference

A data sale trades one dataset between two agents that do not trust each other.
The seller delivers it encrypted over a Beam Room that holds only the two of
them; the buyer checks the delivery against the seller's signed commitments and
pays into an escrow contract on Bittensor EVM; the seller reveals the key; the
buyer decrypts and checks every 4 KiB leaf and challenges a bad one. The user
guide is https://docs.b1m.ai/docs/data-sales.

**Network.** `beam` uses Beam's escrow on Bittensor EVM mainnet (chain 964) by
default, so a sale moves real TAO. Run one only when the user asks for that
sale, at the price they agreed to.

## What a sale guarantees

Say exactly this, and nothing stronger:

- The seller is paid only if nobody shows, within the challenge window, that a
  delivered leaf differs from what the seller committed to. One bad leaf refunds
  the buyer's full price.
- The escrow checks bytes, not meaning. It does not guarantee quality,
  accuracy, legality or usefulness, or that the data is what the seller described.
- Beam is the transport. It decides no dispute and refunds nothing.
- The key becomes public when the seller reveals it. Anyone holding the
  encrypted package can then read the dataset, so the package must reach nobody
  but the buyer.
- The funding transaction makes both addresses, the price, the size, the
  dataset's fingerprint and the deadlines public and permanent.

Never describe a sale as needing no trust at all, never say that Beam vouches
for the data, and never imply that anyone checked the content's quality.

## Conventions

- Pass `--json` on every command. Errors print
  `{"error":{"code":N,"message":"…","hint":"…","kind":"…"}}` on stderr.
- A sale id may be shortened to any unique prefix.
- `sale offer`, `sale buy` and `sale resume --wait` wait for hours by design. Run
  them in the background (OpenClaw: `exec` with `background: true` and a
  `timeoutSeconds` above the wait; Hermes: `terminal` with `background=true`,
  `notify=true` and `persist_on_release=true`) and poll their output.
- A sale belongs to the machine and wallet that started it. Run every step of a
  sale on that machine.

## Before any sale

1. `beam --version` and `beam agent status --json`: the agent must be running.
   Kind `sale_agent_unsupported` means the agent is too old; ask the user before
   running `beam update`.
2. Check the escrow configuration of the current context, and confirm with the
   user that it is the escrow they mean:

   ```bash
   beam config get sale_chain_id
   beam config get sale_rpc_urls
   beam config get sale_escrow
   ```

   Kind `sale_network_unconfigured` means none is set. Ask the user for the
   values; never guess an address.
3. `beam sale wallet show --json`: the address, balances, limits and what was
   spent in the last 24 hours. Kind `sale_wallet_missing`: ask the user before
   `beam sale wallet create --json`. Tell them the new address so a person can
   fund it with a small amount of TAO from a wallet they control.
4. **Keep the wallet small.** The wallet's limits bind commands run through
   `beam`, not other software running as the same OS account, which can read the
   wallet's keys. Advise the user to hold only what they can afford to lose in
   the sale wallet, about one sale's price plus gas, and to refill it from a
   wallet a person holds. Never suggest moving a main wallet's key into it.

## Sell

1. **Agree with the user** on the dataset file, the buyer, the price in TAO and
   its dollar value (1 TAO at most, $10 at least), and the periods (keep the
   defaults unless asked). Recommend compressing the dataset first: leaf hashes
   reveal guessable content (padding, sparse or templated data).
2. **Create the Room** (1 credit, charged to `BEAM_API_KEY`, which needs
   `rooms:start`). Always write the invitation to a file:

   ```bash
   beam sale room create --json --out invitation.token
   ```

   The invitation is a bearer secret. Deliver the file only over a private
   channel the user approves; never print it or paste it into a chat.
3. **Get the buyer's binding**, the file the buyer's `sale join --out` wrote. It
   is not secret. Confirm with the user that its `buyer` address is the buyer
   they agreed to sell to.
4. **Offer** in the background:

   ```bash
   beam sale offer ROOM --json --file dataset.tar.zst --price 0.06 --binding binding.json
   ```

   It refuses to deliver unless the Room holds exactly the seller and the bound
   buyer, the price and size are within limits, the buyer's wallet holds the
   price plus gas, and the delivery budget allows it. It delivers, waits for
   funding until the funding deadline, waits until the funding is in a finalized
   block, reveals the key, and exits with the block after which the sale can be
   settled.
5. **Settle** once that block has passed: `beam sale resume SALE --json`. Run it
   at the `next_block` that `sale status` reports, rather than holding
   `--wait` open for a day.
6. **Revoke** an offer the user no longer wants funded: `beam sale revoke SALE
   --json`. Kind `sale_funded` means the buyer funded first; run
   `beam sale resume SALE --json` at once to reveal the key.

## Buy

1. **Agree with the user** on the seller's address, the dataset, its size and
   the price in TAO and dollars. The payment is held up to about 30 hours with
   the default periods. Say what the sale does not guarantee (above).
2. **Join** with the invitation file the seller sent, and write the binding to a
   file:

   ```bash
   chmod 600 invitation.token
   beam sale join ROOM --json --invitation-file invitation.token --out binding.json
   ```

   Give `binding.json` to the seller, then delete the invitation file.
3. **Check before funding.** `beam sale wallet show --json` must show a balance
   of the price plus about 0.002 TAO for gas (at 5 gwei). `--max-price` must be the price the
   user agreed to, never higher.
4. **Buy** in the background:

   ```bash
   beam sale buy ROOM --json --out dataset.tar.zst --max-price 0.06
   ```

   Before paying it checks the seller's signature, that the offer names this
   wallet, chain and escrow, that the package's file name names the same sale,
   both roots, the size, the price against `--max-price` and the cap, and the
   periods. A package that fails is not
   funded (`sale_package_rejected`, `sale_terms_rejected`): report it, and never
   try to fund it another way. After funding it waits for the key, decrypts,
   and checks every leaf.
5. **Read the outcome.** Every leaf matched: the dataset is at `--out`. A leaf
   did not match: the file is kept as `<out>.unverified`, the client records
   the first bad leaf and challenges; the price comes back. The seller never revealed: the client
   reclaims the payment after the reveal deadline. Tell the user which one.

## Status and resume

```bash
beam sale status --json            # {"sales":[…]} for every sale on this machine
beam sale status SALE --json       # one sale
beam sale resume SALE --json       # do whatever is due, then report
```

Each sale reports `sale_id`, `role`, `price_tao`, `size_bytes`, `counterpart`,
`room_id`, `escrow_state` (`none`, `funded`, `key_released`, `settled`,
`refunded`, `revoked`), `phase`, `outcome` once ended (`settled`, `refunded`,
`revoked`, `expired`, `delivery_failed`), `next` (what to do, in words) and
`next_block` (the block it is due at or must happen by; blocks come about every
12 seconds).

`sale resume` delivers, reveals, settles, reclaims, decrypts or challenges,
whichever is due. It is safe to run again: it resumes a transaction an
interrupted run broadcast instead of sending a new one. After any interruption,
crash or timeout, run `sale status`, then `sale resume`. Never start a second
`sale offer` or `sale buy` for the same sale.

`beam sale withdraw --json` collects a payment the escrow credited because a
direct payment failed; `sale wallet show` lists it as `escrow_credit_tao`.

## Limits that need a person

The wallet defaults are 1 TAO per sale, 3 TAO a day, a person's confirmation
above 0.2 TAO, and a $5 daily budget for Beam charges of deliveries to buyers who
have not funded. The seller also delivers to one unfunded buyer address at a time.

These need a person at an interactive terminal, and no flag stands in for that:

- funding a purchase above the approval threshold;
- raising any limit with `sale wallet limits`;
- `sale wallet send`, which moves TAO out;
- `sale wallet remove`, which deletes the keys.

On kind `sale_approval_required` (exit 10), stop. Tell the user the sale id, the
amount and the counterpart, and ask them to run `beam sale resume SALE` (or the
command that asked) themselves in a terminal, before the funding deadline. On
`sale_limit`, report which limit was hit; only a person may raise it.

## Never

- Never ask for, accept, print, log or store a private key, the wallet secret, or
  an invitation token. Never ask the user to paste a key into the conversation.
  If they want to import a key, they run `beam sale wallet create --import-stdin`
  themselves.
- Never answer a confirmation prompt for the user: no piped `yes`, no `expect`,
  `script` or pseudo-terminal, no rerun in a way meant to skip it.
- Never raise a limit, move TAO out, or remove the wallet.
- Never fund above the price the user agreed to, or buy from an address they did
  not name.
- Never deliver a dataset or its package outside `beam sale offer`, invite anyone
  else into a sale Room, or reuse a sale Room for a different buyer.
- Never edit or delete the records under `sales/` in the Beam configuration
  directory, the package, `<out>.unverified`, or the proof trees while a sale is
  open.
- Never change `sale_chain_id`, `sale_escrow` or `sale_rpc_urls` to values the
  user did not give. The client refuses an escrow that is not the
  DataSaleEscrow v1 build, but it trusts the endpoints: a lying endpoint
  misreports the chain.
- Never start the buyer's agent with `--room-inbox-max-age` or
  `--room-inbox-max-gib` set below the reveal period. The buyer reads the
  delivered package from the agent's inbox until it decrypts it; without it the
  buyer can neither decrypt nor challenge what it paid for.
- Never promise quality, privacy after delivery, or settlement times beyond the
  block deadlines.

## Errors

| Exit | Kind | Do |
| --- | --- | --- |
| 2 | (none) | Bad arguments. Read the message. |
| 2 | `sale_price_too_low`, `sale_price_too_high`, `sale_too_large` | Below $10, above the 1 TAO cap, or above 100 GB. Report it. |
| 3 | `sale_network_unconfigured`, `sale_network_invalid` | Ask the user for the escrow configuration. |
| 3 | `sale_escrow_unverified` | The code at `sale_escrow` is not the DataSaleEscrow v1 build. Stop and tell the user; never point `sale_escrow` at another address yourself. |
| 7 | `sale_agent_unsupported` | Ask the user to approve `beam update`. |
| 8 | `sale_wallet_missing`, `sale_unknown`, `sale_room_unknown`, `sale_no_package` | Create the wallet with approval; list sales with `sale status`; join with `sale join`; ask the seller to offer, then buy again. |
| 8 | `sale_package_gone` | The agent removed the delivered package. Tell the user; the sale can no longer be decrypted or challenged. |
| 9 | `sale_room_not_private` | The Room holds someone else. Create a new sale Room for this buyer. |
| 9 | `sale_buyer_has_unfunded_offer`, `sale_buyer_cannot_pay` | Nothing was delivered. Wait for the earlier offer to be funded or expire, or for the buyer to fund its wallet. |
| 9 | `sale_funded` | Run `sale resume` now to reveal the key. |
| 9 | `sale_commitments_mismatch`, `sale_key_mismatch`, `sale_record_mismatch` | Stop and tell the user; the key is not revealed. |
| 9 | `sale_already_recorded` | This machine already took up that sale. Run `sale resume`. |
| 10 | `sale_approval_required`, `sale_limit` | Hand over to a person (above). |
| 10 | `sale_package_rejected`, `sale_terms_rejected` | Not funded. Report it. |
| 10 | `sale_wallet_insufficient`, `sale_wallet_no_gas` | Tell the user the address and amount to fund, then `sale resume`. |
| 10 | `sale_rpc_unreachable` | No endpoint answered. Suggest adding an endpoint to `sale_rpc_urls`, then `sale resume`. |
| 10 | `sale_account_busy` | `sale revoke` or `sale withdraw` found transactions this client did not send pending. Wait, then run it again. A running sale waits for them on its own. |
| 10 | `sale_too_late`, `sale_deadline_passed`, `sale_reverted`, `sale_would_revert` | Run `sale status` and report the escrow state. |
| 130 | (none) | A person declined the confirmation. Do not retry. |

The CLI says when an RPC endpoint asked it to slow down (HTTP 429) and waits on
its own. Public endpoints limit each address; an agent that runs many sales
should have the user configure an endpoint of its own first in `sale_rpc_urls`.

## Costs

- Gas, measured on testnet at 5 gwei: about 164,000 gas (0.0008 TAO) for an
  honest sale. The buyer pays funding (about 87,000 gas), the seller the reveal
  and settlement (about 77,000). A challenge is 150,000 to 176,000 gas; a
  reclaim about 40,000; a revocation about 49,000.
- Beam charges the seller's organization: 1 credit for the sale Room, and 0.2
  credit plus 0.02 credit per GB for each delivery. A credit is $0.50. The buyer
  pays Beam nothing.
- The escrow takes no fee. Tell the user the price, the gas and the Beam
  charges before they agree to a sale.
