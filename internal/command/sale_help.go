package command

// The data-sale help topic. It lives beside the sale commands rather than in help_topics.go, so it goes wherever they go.
func init() {
	helpTopics["sale"] = helpTopic{
		Usage:   []string{"beam sale <command> [options]"},
		Summary: "Sell or buy a dataset between agents that do not trust each other: the seller delivers it encrypted over a Room, the buyer pays into an escrow on Bittensor EVM, and the escrow pays the seller only if no leaf of the delivery can be shown to differ from what the seller committed to.",
		Sections: []helpSection{
			{Title: "Seller", Entries: []helpEntry{
				{"room create [--api-key <key>] [--max-size <size>] [--invitation-ttl <duration>] [--out <file>]", "Create a Room for one buyer and its one-use invitation"},
				{"offer <room-id> --file <dataset> --price <tao> --binding <file> [options]", "Deliver the dataset encrypted, then reveal the key once the buyer funds"},
				{"revoke <sale-id>", "Withdraw an offer nobody funded"},
			}},
			{Title: "Buyer", Entries: []helpEntry{
				{"join <room-id> (--invitation-file <file> | --invitation-token <token>) [--out <file>]", "Join the seller's Room and sign the binding the seller's offer needs"},
				{"buy <room-id> --out <file> --max-price <tao> [--sale <sale-id>] [--wait <duration>]", "Check the delivery, fund, decrypt and check every leaf, and challenge a bad one"},
			}},
			{Title: "Both", Entries: []helpEntry{
				{"status [<sale-id>]", "List the sales on this machine with their escrow state and next step"},
				{"resume <sale-id> [--wait]", "Carry a sale forward: reveal, settle, reclaim, decrypt or challenge, as due"},
				{"withdraw", "Collect a payment the escrow could not send directly"},
				{"wallet create [--import-stdin]", "Create the sale wallet, or import its key from standard input"},
				{"wallet show", "Show the address, balances, limits and what was spent in the last 24 hours"},
				{"wallet backup", "Add a backup account that sends reveals and refunds when the main one is stuck"},
				{"wallet limits [--per-sale <tao>] [--daily <tao>] [--approve-above <tao>] [--delivery-budget <usd>]", "Show or change the spending limits"},
				{"wallet send --to <address> (--amount <tao> | --all)", "Move TAO out of the sale wallet"},
				{"wallet remove", "Delete an empty wallet's keys"},
			}},
			{Title: "Offer options", Entries: []helpEntry{
				{"--reveal-period <blocks>", "Blocks the seller has to reveal the key once funded (default 1800, about 6 h)"},
				{"--challenge-period <blocks>", "Blocks the buyer has to challenge once the key is revealed (default 7200, about 24 h)"},
				{"--fund-within <blocks>", "Blocks the buyer has to fund after the delivery window (default 1800)"},
				{"--tao-usd <price>", "The TAO/USD price for the $10 minimum, instead of a fetched quote"},
				{"--package-dir <dir>", "Where to write the encrypted package (default: beside the dataset)"},
				{"--name <name>", "The dataset name the buyer sees (default: the file name)"},
			}},
		},
		Notes: []string{
			"A sale runs in three transactions: the buyer funds the escrow with the seller's signed offer, the seller reveals the key, and the seller settles once the challenge window closes. If the seller never reveals, the buyer reclaims the payment after the reveal period; if a leaf decrypts to anything but what the seller committed to, the buyer's challenge refunds it.",
			"The escrow guarantees that the bytes match the seller's commitments, not that the data is what the buyer wanted. The key becomes public when it is revealed, so the dataset is private only as long as the encrypted package reaches nobody but the buyer: the sale Room holds only the seller and the buyer, and the package goes to the buyer's member only.",
			"Limits: 1 TAO per sale at most (the escrow's cap), $10 at least, and 100 GB. The wallet's own limits default to 1 TAO per sale, 3 TAO a day, a person's confirmation above 0.2 TAO, and $5 a day of Beam charges for deliveries to buyers who have not funded. Raising a limit, approving a spend and moving TAO out always need a person at a terminal; no flag stands in for that. These limits bind an agent that runs beam, not software running as the wallet's OS account, which can read its keys: keep the wallet's balance small.",
			"A release build uses Beam's DataSaleEscrow v1 deployment on Bittensor EVM mainnet (chain 964) by default, and a development build its testnet deployment. A context can select another with `beam config set sale_chain_id`, `sale_rpc_urls` (comma-separated, in order of preference) and `sale_escrow`. The client refuses an escrow whose code is not the DataSaleEscrow v1 build, and trusts what the endpoints report, so use endpoints you trust. The buyer's agent must keep the delivered package until it is decrypted: do not run it with an inbox age or size limit shorter than the reveal period. The wallet's keys stay in the operating-system keyring, or an owner-only file where there is none, and are never printed.",
		},
	}
}
