---
feature: notes
paths:
  - app/server.js
  - app/notes/**
flows:
  - add-note
---

# Notes

## What exists

A notes page with a form and a list. Each note submitted through the form is
kept in memory for the lifetime of the service instance.

## How a user reaches it

Follow the Notes link in the navigation bar, or open `notes` under the service
root.

## How to drive it

Run the `add-note` flow. It fills the note field, clicks Add note, then takes a
snapshot, a screenshot and the request log, which shows the redirect after the
form post.

## What usually breaks

The form post does not redirect back to the list, the new note is missing from
the list, or a note containing markup is not escaped.
