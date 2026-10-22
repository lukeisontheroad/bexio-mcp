# bexio API conventions

This connector reaches one bexio company: the one chosen at sign-in. Every
call is a live request against that company's books.

## Workflow

1. `bexio_docs` with a group name (e.g. `Contacts`) lists its operations;
   with an operationId it shows path/query parameters and the request body.
2. `bexio_read` runs GET and search operations, `bexio_write` everything else.
3. Before any write, tell the user exactly what will change and wait for a
   yes. Issuing, sending and deleting have effects outside the chat
   (customer e-mails, permanent deletion, bookings).

## Finding records

- Prefer search operations (`v2SearchContact`, `v2SearchInvoices`, ...) over
  lists. Body: JSON array of `{"field", "value", "criteria"}`, all ANDed.
  Criteria: `=`, `!=`, `>`, `>=`, `<`, `<=`, `like`, `not_like`, `is_null`,
  `not_null`, `in`, `not_in`. `like` needs explicit `%` wildcards
  (`"%acme%"`).
- List/search query parameters: `limit` (50 unless set, max 2000),
  `offset`, `order_by` (comma-separated, `_desc` suffix, e.g. `id_desc`),
  `show_archived`. Long results are truncated; page with `limit`/`offset`.
- Signed-in user: `v3ShowMe`. Company: `v2ListCompanyProfile`.

## Writing (2.0 API)

- Edit is POST and partial: send only the fields that change.
- Creating contacts and documents needs `user_id` (contacts also
  `owner_id`): use the id from `v3ShowMe` unless the user names someone else.
- Contacts: `contact_type_id` 1 = company, 2 = person; for a person `name_1`
  is the last name and `name_2` the first name. `contact_group_ids` and
  `contact_branch_ids` are comma-separated strings, not arrays. Don't send
  the deprecated `address`; use `street_name` + `house_number` +
  `address_addition`.
- Deleting a contact archives it (it can be restored); deleting quotes,
  orders, invoices or projects is permanent.
- Item prices are decimal strings.
- Notes, tasks and invoices link a project via the writable `pr_project_id`;
  responses show the read-only `project_id`.
- Timesheets take a nested `tracking` object, either
  `{"type": "duration", "date", "duration"}` or
  `{"type": "range", "start", "end"}`.

## Sales documents (quotes `kb_offer`, orders `kb_order`, invoices `kb_invoice`)

- Positions use shared operations with `kb_document_type` (`kb_offer`,
  `kb_order`, `kb_invoice`) and `document_id` path parameters. Taxes valid
  for positions: `ListTaxes` with query `types=sales_tax`, `scope=active`.
- Status ids (`kb_item_status_id`, read-only): quotes 1 draft, 2 pending,
  3 confirmed, 4 declined; orders 5 pending, 6 done, 15 partial,
  21 canceled; invoices 7 draft, 8 pending, 9 paid, 16 partial, 19 canceled,
  31 unpaid; deliveries 10 draft, 18 done, 20 canceled.
- Actions (issue, accept, reject, cancel, mark as sent, revert) take no body
  and return `{"success": true}`.
- Sending by e-mail needs `recipient_email`, `subject` and `message`; the
  message must contain the literal placeholder `[Network Link]`.
- Copying a document requires `contact_id`.

## 3.0 and 4.0 APIs

- 3.0 lists page with `limit`/`offset` only (no `order_by`).
- 4.0 (bills, expenses, outgoing payments) uses UUID string ids, wraps lists
  in `{"data": [...], "paging": {...}}` with `limit` + `page`, has no search
  (filter with query parameters), answers deletes with an empty 204, and its
  PUT/PATCH updates replace the whole object: fetch the record, change the
  fields, send it back complete.
- Manual entries (accounting) are full-replace PUT too, with a nested
  `entries` array of debit/credit lines.

## Not available here

Bank payments, payroll, PDF and file downloads, and file uploads. For PDFs,
point the user to the document in bexio.

## Errors

bexio answers `{"error_code": ..., "message": ...}`. 403 usually means the
connector was not granted that area at sign-in; 429 is the per-company rate
limit: wait a minute and retry.
