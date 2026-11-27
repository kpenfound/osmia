---
feature: home
paths:
  - app/server.js
flows:
  - home-page
---

# Home

## What exists

A landing page with a heading, a navigation bar and the instance token. The
token is a random value the service generates when it starts, so every new
service instance shows a different one.

## How a user reaches it

Open the service root. The Home link in the navigation bar returns to it.

## How to drive it

Run the `home-page` flow. It loads the page, takes an accessibility snapshot
and a screenshot, then reads the console and the requests.

## What usually breaks

The page answers with an HTTP error, the console reports an error, or the
instance token is missing from the page.
