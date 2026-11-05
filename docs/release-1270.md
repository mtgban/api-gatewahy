# Release order for the stepped add-ons

The order that keeps the pricing page and the Stripe bill in agreement. The
gateway already pins the current site master through the newspaper PR, so the
re-pin in step 3 waits for the site's second PR.

1. Merge the gateway PRs #96, #97 and the stepped-addons PR, in that order,
   and deploy the gateway. Stripe is unchanged by this deploy: no seed runs,
   and the pinned list has no `first_monthly` values. From this deploy, new
   All Access keys carry the newspaper mode and the trial grants it;
   `history_on_sale` stays off.
2. Merge the site's second PR (the 199/499/799 amounts, `first_monthly` on
   `extra_store` and `extra_game`, the stepped page total). Do not tag a site
   deploy yet.

   Before step 3, list the live subscriptions holding any package item or
   an `extra_store_*` or `extra_game_*` item (the Stripe dashboard's
   Subscriptions list; the gateway has no listing of its own). If any
   belongs to a real customer rather than a tester, stop: the plan-change
   proration compares catalog totals, so a subscription still on an
   archived flat add-on Price would be judged by the stepped amounts, not
   what it bills. Migrate those subscriptions, or change
   `prorationBehavior` to read the items' own Prices, first.
3. Merge the one-line PR re-pinning the gateway to that site commit and
   build it. From that build, run `api-gatewahy catalog seed` in test mode,
   then in live mode; only then deploy the gateway. Between the seed and
   the deploy the running gateway quotes the old, higher amounts against
   the new Prices, which only over-quotes. Deploying first would
   under-quote the confirm page, and a plan change in that window would
   add items at flat Prices the seed then archives for good. Expected
   seed results:
   - ten replacement Prices and ten archived (three packages and two
     counted add-ons at two intervals);
   - two newspaper Prices created if the newspaper was never seeded, or
     two replacements if it was seeded at the old placeholder amount;
   - two history Prices created if live Stripe was last seeded before the
     catalog carried the history package (history stays off sale either
     way).

   In the Stripe dashboard, `extra_store_*` and `extra_game_*` are
   graduated with two tiers each.
4. Tag the site deploy, then set `api_gateway.newspaper_on_sale` on the
   sites.

Between steps 3 and 4 the page quotes the old, higher amounts while Stripe
bills the new ones, which only over-quotes, never the reverse.
