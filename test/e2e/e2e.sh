#!/usr/bin/env bash
#
# End-to-end test of a RUNNING connector against a real Jiku and a real GitHub repo + Project.
# It acts as a person on both sides and waits for the connector to mirror each action.
#
# Never point it at a production repo or Jiku project: it creates issues, comments, moves cards,
# closes issues and changes requirements. Use a test repo, a test Project with the same field
# mapping, and a test Jiku project.
#
# Requirements: jq, gh, the `jiku` CLI from jiku-go, and the connector running with SYNC_CONFIG
# mapping $E2E_JIKU_PROJECT to $E2E_REPO and $E2E_GH_PROJECT.
#
# Variables:
#   E2E_JIKU_PROJECT     Jiku project id mapped in the connector's config
#   E2E_REPO             owner/name of the mapped test repo (its defaultRepo)
#   E2E_GH_PROJECT       node id of the mapped Project v2
#   E2E_GH_TOKEN         GitHub token of the "person" (repo + project scopes)
#   E2E_JIKU_KEY_FILE    machine user key of the "person" in Jiku (NOT the connector's identity:
#                        the connector ignores its own writes)
#   JIKU_SERVERS JIKU_INSTANCE JIKU_CREDS JIKU_ISSUER JIKU_PROJECT_ID   bus settings
#   JIKU_BIN             path to the jiku CLI (default: jiku)
#   E2E_WEBHOOK_URL E2E_WEBHOOK_SECRET   optional: also test the webhook receiver
#   E2E_TIMEOUT          seconds to wait for each mirror (default 120)
#
# The field names follow the connector's example board (config.example.yaml); change the
# E2E_OPT_* variables to match the test Project's options.
set -euo pipefail

: "${E2E_JIKU_PROJECT:?}" "${E2E_REPO:?}" "${E2E_GH_PROJECT:?}" "${E2E_GH_TOKEN:?}" "${E2E_JIKU_KEY_FILE:?}"
JIKU_BIN=${JIKU_BIN:-jiku}
TIMEOUT=${E2E_TIMEOUT:-120}
OPT_INBOX=${E2E_OPT_INBOX:-Inbox}
OPT_REVIEW=${E2E_OPT_REVIEW:-Review}
OPT_PROGRESS=${E2E_OPT_PROGRESS:-In Progress}
OPT_STUCK=${E2E_OPT_STUCK:-Blocked}
OPT_CRITICAL=${E2E_OPT_CRITICAL:-Critical}
TYPE_BUG=${E2E_TYPE_BUG:-Bug}
TYPE_FEATURE=${E2E_TYPE_FEATURE:-Feature}
RUN=$(date +%s)
# Only issues created during this run count: a test environment whose Jiku was reset reuses
# requirement ids, and old closed issues may carry the same marker.
RUN_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)

pass=0; fail=0
ok()   { pass=$((pass+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { fail=$((fail+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }

jk()  { JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE "$JIKU_BIN" "$@"; }
ghx() { GH_TOKEN=$E2E_GH_TOKEN gh "$@"; }

# wait_for <description> <command...>: retries the command until it prints something non-empty.
wait_for() {
  local what=$1; shift
  local deadline=$((SECONDS + TIMEOUT)) out
  while (( SECONDS < deadline )); do
    out=$("$@" 2>/dev/null || true)
    if [[ -n "$out" && "$out" != "null" ]]; then ok "$what"; echo "$out" > "$LAST"; return 0; fi
    sleep 3
  done
  bad "$what (timeout ${TIMEOUT}s)"; return 1
}
LAST=$(mktemp)
last() { cat "$LAST"; }
trap 'rm -f "$LAST"' EXIT

req()        { jk query requirements.get --id "$1" --include description -o json; }
req_field()  { req "$1" | jq -r ".$2"; }
jcomments()  { jk query comments.list --filter entityType=requirement --filter "entityId=$1" --all -o json; }
issue()      { ghx api "repos/$E2E_REPO/issues/$1"; }
gcomments()  { ghx api "repos/$E2E_REPO/issues/$1/comments"; }
item_field() { # <issue number> <field name>
  ghx api graphql -f query='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){projectItems(first:10){nodes{project{id} fieldValueByName(name:"'"$2"'"){... on ProjectV2ItemFieldSingleSelectValue{name}}}}}}}' \
    -f o="${E2E_REPO%/*}" -f r="${E2E_REPO#*/}" -F n="$1" \
    | jq -r --arg p "$E2E_GH_PROJECT" '.data.repository.issue.projectItems.nodes[]|select(.project.id==$p)|.fieldValueByName.name // empty'
}
set_status() { # <issue number> <option name>
  local item field opt
  item=$(ghx api graphql -f query='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){projectItems(first:10){nodes{id project{id}}}}}}' \
    -f o="${E2E_REPO%/*}" -f r="${E2E_REPO#*/}" -F n="$1" | jq -r --arg p "$E2E_GH_PROJECT" '.data.repository.issue.projectItems.nodes[]|select(.project.id==$p)|.id')
  read -r field opt < <(ghx api graphql -f query='query($id:ID!){node(id:$id){... on ProjectV2{field(name:"Status"){... on ProjectV2SingleSelectField{id options{id name}}}}}}' -f id="$E2E_GH_PROJECT" \
    | jq -r --arg o "$2" '.data.node.field as $f|"\($f.id) \($f.options[]|select(.name==$o)|.id)"')
  ghx api graphql -f query='mutation($p:ID!,$i:ID!,$f:ID!,$o:String!){updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f,value:{singleSelectOptionId:$o}}){projectV2Item{id}}}' \
    -f p="$E2E_GH_PROJECT" -f i="$item" -f f="$field" -f o="$opt" >/dev/null
}
issue_of_req() { # finds the issue created for a requirement by its marker, during this run
  ghx api "repos/$E2E_REPO/issues?state=all&per_page=50&sort=created&direction=desc" \
    | jq -r --arg m "jiku-github-sync:requirement=$1 -->" --arg s "$RUN_START" \
      '.[]|select(.created_at >= $s and .body!=null and (.body|contains($m)))|.number' | head -1
}
req_of_issue() { # finds the requirement created for an issue by title in the project
  jk query requirements.list --filter "projectId=$E2E_JIKU_PROJECT" --all -o json | jq -r --arg t "$1" '.[]|select(.title==$t)|.id' | head -1
}

step "R1 · requisito público nuevo en Jiku → issue"
R=$(jk cmd requirements.new "$(jq -nc --arg t "e2e $RUN incidencia desde Jiku" --argjson p "$E2E_JIKU_PROJECT" \
  '{title:$t,description:"Creado por el e2e.\n\n| a | b |\n|---|---|\n| 1 | 2 |",projectId:$p,type:"incidencia",priority:"urgente",state:"desarrollo",visibilityLevel:"public"}')" -o json | jq -r .id)
wait_for "issue creado para el requisito $R" issue_of_req "$R"; N=$(last)
[[ $(issue "$N" | jq -r '.type.name // ""') == "$TYPE_BUG" ]] && ok "tipo $TYPE_BUG" || bad "tipo del issue"
out=""; for _ in $(seq 1 $((TIMEOUT/3))); do out=$(item_field "$N" Status); [[ "$out" == "$OPT_PROGRESS" ]] && break; sleep 3; done
[[ "$out" == "$OPT_PROGRESS" ]] && ok "Status sigue a Jiku: $out" || bad "Status: '$out'"
[[ $(item_field "$N" Priority) == "$OPT_CRITICAL" ]] && ok "Priority = $OPT_CRITICAL" || bad "Priority: '$(item_field "$N" Priority)'"

step "R6 · comentario público de Jiku → issue; el interno no sale"
jk cmd "requirements.$R.comment" '{"comment":"comentario público e2e","visibilityLevel":"public"}' -o json >/dev/null
jk cmd "requirements.$R.comment" '{"comment":"comentario interno e2e","visibilityLevel":"internal"}' -o json >/dev/null
wait_for "comentario público espejado en GitHub" sh -c "GH_TOKEN=$E2E_GH_TOKEN gh api repos/$E2E_REPO/issues/$N/comments | jq -r '.[]|select(.body|contains(\"comentario público e2e\"))|.id'"
sleep 5
[[ -z $(gcomments "$N" | jq -r '.[]|select(.body|contains("comentario interno e2e"))|.id') ]] && ok "el interno no salió" || bad "el interno llegó a GitHub"

step "Edición en Jiku (título) → GitHub; edición en GitHub (cuerpo) → Jiku"
jk cmd "requirements.$R.edit" "$(jq -nc --arg t "e2e $RUN título editado en Jiku" '{title:$t}')" -o json >/dev/null
wait_for "título en GitHub" sh -c "GH_TOKEN=$E2E_GH_TOKEN gh api repos/$E2E_REPO/issues/$N | jq -r 'select(.title|contains(\"editado en Jiku\"))|.number'"
body=$(issue "$N" | jq -r .body)
ghx api -X PATCH "repos/$E2E_REPO/issues/$N" -f body="${body/Creado por el e2e./Cuerpo editado en GitHub.}" >/dev/null
wait_for "descripción en Jiku, sin el marcador" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query requirements.get --id $R --include description -o json | jq -r 'select((.description|startswith(\"Cuerpo editado en GitHub.\")) and (.description|contains(\"jiku-github-sync\")|not))|.id'"

step "Estados: tarjeta movida en GitHub → Jiku; Stuck; cierre de incidencia"
set_status "$N" "$OPT_REVIEW"
wait_for "requisito en revision" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query requirements.get --id $R -o json | jq -r 'select(.state==\"revision\")|.id'"
set_status "$N" "$OPT_STUCK"
wait_for "Stuck → en_cola" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query requirements.get --id $R -o json | jq -r 'select(.state==\"en_cola\")|.id'"
wait_for "comentario interno de Stuck" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query comments.list --filter entityType=requirement --filter entityId=$R --all -o json | jq -r '.[]|select(.visibilityLevel==\"internal\" and (.body|contains(\"Stuck\")))|.id'"
sleep 20
[[ $(item_field "$N" Status) == "$OPT_STUCK" ]] && ok "la tarjeta sigue en $OPT_STUCK (sin rebote)" || bad "rebote: $(item_field "$N" Status)"
ghx api -X PATCH "repos/$E2E_REPO/issues/$N" -f state=closed -f state_reason=completed >/dev/null
wait_for "incidencia resuelta con resolución" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query requirements.get --id $R --include resolutionType --include resolutionConclusion -o json | jq -r 'select(.state==\"resuelto\" and .resolutionType==\"otro\" and (.resolutionConclusion|contains(\"Cerrado en GitHub\")))|.id'"
jk cmd "requirements.$R.edit" '{"state":"desarrollo"}' -o json >/dev/null
wait_for "reabrir en Jiku reabre el issue" sh -c "GH_TOKEN=$E2E_GH_TOKEN gh api repos/$E2E_REPO/issues/$N | jq -r 'select(.state==\"open\")|.number'"

step "R2 · issue nuevo en GitHub → requisito público"
T="e2e $RUN pedido desde GitHub"
N2=$(ghx api -X POST "repos/$E2E_REPO/issues" -f title="$T" -f body="Pedido abierto en GitHub por el e2e." -f type="$TYPE_FEATURE" --jq .number)
wait_for "requisito creado" req_of_issue "$T"; R2=$(last)
[[ $(req_field "$R2" visibilityLevel) == public && $(req_field "$R2" type) == funcionalidad ]] && ok "público y funcionalidad" || bad "visibilidad/tipo: $(req "$R2" | jq -c '{visibilityLevel,type}')"
out=""; for _ in $(seq 1 $((TIMEOUT/3))); do out=$(item_field "$N2" Status); [[ -n "$out" ]] && break; sleep 3; done
[[ "$out" == "$OPT_INBOX" ]] && ok "Status $out" || bad "Status: '$out'"

step "R7/R8 · comentario de GitHub → Jiku, edición y borrado"
C=$(ghx api -X POST "repos/$E2E_REPO/issues/$N2/comments" -f body="comentario de GitHub e2e" --jq .id)
wait_for "comentario espejado en Jiku (público, con atribución)" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query comments.list --filter entityType=requirement --filter entityId=$R2 --all -o json | jq -r '.[]|select(.visibilityLevel==\"public\" and (.body|contains(\"issuecomment-$C\")) and (.body|contains(\"comentario de GitHub e2e\")))|.id'"
ghx api -X PATCH "repos/$E2E_REPO/issues/comments/$C" -f body="comentario de GitHub e2e (editado)" >/dev/null
wait_for "edición espejada" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query comments.list --filter entityType=requirement --filter entityId=$R2 --all -o json | jq -r '.[]|select(.body|contains(\"(editado)\"))|.id'"
ghx api -X DELETE "repos/$E2E_REPO/issues/comments/$C" >/dev/null
wait_for "borrado marcado en Jiku" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query comments.list --filter entityType=requirement --filter entityId=$R2 --all -o json | jq -r '.[]|select(.body|contains(\"comentario eliminado en GitHub\"))|.id'"

step "Adjuntos · imagen de Jiku → GitHub (embebida); imagen de GitHub → Jiku (archivo)"
PNG=$(mktemp --suffix=.png); trap 'rm -f "$LAST" "$PNG"' EXIT
# A 1x1 PNG.
printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde\x00\x00\x00\x0cIDATx\x9cc\xf8\xcf\xc0\x00\x00\x03\x01\x01\x00\xc9\xfe\x92\xef\x00\x00\x00\x00IEND\xaeB`\x82' > "$PNG"
ticket=$(jk cmd files.request-upload "$(jq -nc --argjson s "$(stat -c%s "$PNG")" '{fileName:"adjunto-e2e.png",mimeType:"image/png",fileSize:$s}')" -o json)
F=$(jq -r .id <<<"$ticket")
curl -sf -X PUT -H 'Content-Type: image/png' --data-binary @"$PNG" "$(jq -r .uploadUrl <<<"$ticket")" >/dev/null
jk cmd "requirements.$R.comment" "$(jq -nc --arg c "adjunto e2e jiku ![file:$F]" --argjson f "$F" '{comment:$c,visibilityLevel:"public",fileIds:[$f]}')" -o json >/dev/null
wait_for "imagen de Jiku embebida en GitHub" sh -c "GH_TOKEN=$E2E_GH_TOKEN gh api repos/$E2E_REPO/issues/$N/comments | jq -r '.[]|select((.body|contains(\"adjunto e2e jiku ![adjunto-e2e.png](https://github.com/user-attachments/assets/\")))|.id'"
RID=$(ghx api "repos/$E2E_REPO" --jq .id)
U=$(curl -sf -X POST "https://uploads.github.com/user-attachments/assets?name=captura-e2e.png&content_type=image/png&repository_id=$RID" \
  -H "Authorization: Bearer $E2E_GH_TOKEN" -H 'Accept: application/json' --data-binary @"$PNG" | jq -r .url)
ghx api -X POST "repos/$E2E_REPO/issues/$N2/comments" -f body="adjunto e2e github ![captura]($U)" >/dev/null
wait_for "imagen de GitHub copiada como archivo de Jiku" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query comments.list --filter entityType=requirement --filter entityId=$R2 --all -o json | jq -r '.[]|select((.body|contains(\"adjunto e2e github ![file:\")) and (.attachments|length)==1 and .attachments[0].fileName==\"captura.png\")|.id'"

if [[ -n "${E2E_WEBHOOK_URL:-}" && -n "${E2E_WEBHOOK_SECRET:-}" ]]; then
  step "Webhook firmado → sync inmediato"
  ghx api -X PATCH "repos/$E2E_REPO/issues/$N2" -f title="$T (webhook)" >/dev/null
  payload=$(jq -nc --arg r "$E2E_REPO" --argjson n "$N2" '{action:"edited",issue:{number:$n},repository:{full_name:$r}}')
  sig="sha256=$(printf '%s' "$payload" | openssl dgst -sha256 -hmac "$E2E_WEBHOOK_SECRET" | awk '{print $NF}')"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$E2E_WEBHOOK_URL" -H "X-GitHub-Event: issues" -H "X-Hub-Signature-256: $sig" -d "$payload")
  [[ $code == 202 ]] && ok "entrega aceptada" || bad "entrega: HTTP $code"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$E2E_WEBHOOK_URL" -H "X-GitHub-Event: issues" -H "X-Hub-Signature-256: sha256=00" -d "$payload")
  [[ $code == 401 ]] && ok "firma inválida rechazada" || bad "firma inválida: HTTP $code"
  wait_for "título por webhook" sh -c "JIKU_KEY_FILE=$E2E_JIKU_KEY_FILE $JIKU_BIN query requirements.get --id $R2 -o json | jq -r 'select(.title|endswith(\"(webhook)\"))|.id'"
fi

step "R4 · requisito que pasa a interno → se desvincula"
jk cmd "requirements.$R2.edit" '{"visibilityLevel":"internal"}' -o json >/dev/null
wait_for "aviso de desvinculación en el issue" sh -c "GH_TOKEN=$E2E_GH_TOKEN gh api repos/$E2E_REPO/issues/$N2/comments | jq -r '.[]|select(.body|contains(\"dejó de sincronizarse\"))|.id'"

printf '\n%d ok, %d fallas\n' "$pass" "$fail"
echo "Requisitos: $R $R2 · issues: #$N #$N2"
(( fail == 0 ))
