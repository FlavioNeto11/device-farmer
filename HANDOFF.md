# Estado da sessão — retomar daqui

Atualizado em 2026-09-04, depois de fechar tudo o que a pausa anterior deixou
aberto. Este arquivo é o único lugar que reúne o estado; leia-o antes de
qualquer coisa.

## Onde o repositório está

- `main` = `4af645d`, **empurrado para o origin**. Árvore limpa, sem worktrees.
- Remote: `https://github.com/FlavioNeto11/device-farmer`
- Schema **v11**, contígua (`00001`…`00011`). 121 arquivos Go, ~90k linhas,
  **493 testes**, **5 suítes de asserção SQL**.
- Registro de requisitos: `REQUIREMENTS.md` e aba **Docs → Requirements**.

### Verificado nesta sessão, de ponta a ponta

```
go build ./... && go vet ./... && gofmt -l .     # limpos
go test -count=1 ./...                           # 493 PASS
farmd migrate up  (banco vazio → v11)            # 11 migrations
test/assertions*.sql                             # 5 suítes, todas PASSED
```

Contra um farm vivo (56 devices simulados, schema v11): `healthz` 200,
`/api/v1/capabilities` **401 sem credencial e 200 com**, `/api/v1/reaper` 200,
aba Docs 200 em todas as 7 áreas, dashboard 200, **99 nomes de métrica** em
`/metrics` (eram 15).

## O que foi fechado desde a pausa

**Os 9 PRs foram mergeados** (`gh pr view` reporta MERGED em todos), com a
numeração de migration renumerada para contígua antes de cada merge — `farmd
migrate up` chama goose sem `WithAllowMissing`, então um buraco trava migrações
futuras.

**As 8 worktrees interrompidas foram recuperadas**, todas com os gates rodados
contra o HEAD atual e não contra a base defasada delas:

| Unidade | O que entrou |
|---------|--------------|
| 23 | Testes de `reaper` e `scheduler` — 3.562 linhas, 128 casos |
| 24 | Testes de `runner` e `jobrunner` — 3.904 linhas, mais o seam `leaseHolder` |
| 18 | Kill switch do reaper (HTTP + `ctl` + audit) e exit code do `shell_detached` |
| 22 | 19 alertas, 16 runbooks, `farm_api_auth_open` |
| 12 | `resolve_device` → `ambiguous` (00011), bulk exclui doente/quarentenado, `holder_instance` retido |
| 5 | Charge gate no agente — set-point de VBUS com dead-man's switch |
| 27 | Proxy de fence — desenho e esqueleto inertes, 24 testes |
| 2 | Inventário de gaps reverificado |

**`internal/api/capabilities.go` foi corrigido** — as três sondas devolvem erro,
um relatório que não pôde ser tirado é 503 dizendo o que não pôde observar, e a
rota passou a ser gated em `tenant`.

**O registro de requisitos foi reverificado**: 36 das 101 linhas estavam
erradas, porque ele foi escrito sobre `ca91c1c` e a migration `00005` mais nove
branches entraram por baixo. 43 linhas em `met` (eram 30).

**O inventário de gaps caiu de 88 para 46**, em duas passadas.

## O que fazer ao retomar, nesta ordem

A lista vem do próprio registro (`REQUIREMENTS.md` → "What the register argues
for next"), ordenada por desbloqueio-por-unidade-de-trabalho:

1. **`LEASE-09` — dar partida no witness loop.** O loop está escrito, o marcador
   no device está escrito, a função SQL está escrita, a config é validada, e
   **todo call site de `StartWitness` na árvore é teste**. É a última metade não
   ligada da contramedida ao #663: sem ela, uma queda de control plane maior que
   TTL+grace ainda tira o device de um job que está funcionando. Um call site no
   jobrunner.
2. **`LEASE-11` — kill switch alcançável para o `max_runtime`.** O do reaper
   ficou pronto (unidade 18); `lease_expire_max_runtime` continua sem responder
   a ele nem ao quiesce. Se DEVE responder é pergunta de desenho ainda não
   respondida por escrito: `max_runtime` é prazo que o usuário escreveu.
3. **`REC-12` — já fechado**; o que sobrou é `farm.quarantines.scope` aceitar
   `slot` e `power_domain` sem nenhum escritor.
4. **A malha de charge policy (`HW-03`, `DEV-09`).** As duas peças que faltavam
   existem — o leitor de bateria e o estado `parked` — e o gate do agente também.
   Falta **o loop entre eles** (`internal/chargepolicy`, unidade 6, nunca
   disparada). É a única linha do registro em que uma mitigação física de
   segurança espera software.
5. **`SEC-04` — integrar o proxy de fence** (unidade 28, nunca disparada). O
   desenho e o esqueleto estão em `docs/design/fence-proxy.md` e
   `internal/fenceproxy`; nada constrói nenhum dos dois.
6. **`JOB-06`/`JOB-07` — `profile_id` e os três irmãos, escritos pela API.**
7. **`OPS-07` — Deployment do `janitor` no chart.** É o único papel sem um, e é o
   único que fecha `farm.job_steps` de processo morto: um deploy Kubernetes a
   partir deste chart acumula linhas órfãs que envenenam o claim do jobrunner.
8. **`SEC-07` — escopar as seis superfícies de leitura ainda sem tenant.**
   `/events` foi fechado; `/fleet`, `/topology`, `/hosts`, `/recovery`, `/bulk`
   e `/stream` não. O stream precisa de um poller por tenant.
9. **`TEST-04`/`TEST-05` — testes para `isRetryable` e `planResume`.**

Unidades do plano nunca disparadas: 6 (política de carga 40–80%), 7 (saúde e
inchaço de bateria), 20 (knobs do `topo`), 21 (superfície de operador para
slots), 28 (integração do proxy de fence).

## Defeitos conhecidos e ainda abertos em `main`

- **`internal/enroll` e `internal/topo` não têm nenhum teste.** São o caminho
  pelo qual um aparelho entra na frota.
- **`Disposition.Escalate()` não tem chamador em produção** — o veredito do
  atuador existe e não governa a escalada da escada.
- **`obs.OutcomeRefusedGanged` nunca é emitido na prática**, então uma recusa
  por hub ganged é indistinguível de recusa por política nas métricas.
- **O corpo do revoke ainda diz "refused at the host proxy"** e o proxy não está
  no caminho de nada. É a última das três "mensagens de remédio errado".
- **`farm.quarantines.scope` aceita `slot` e `power_domain`** e nenhum código
  escreve qualquer um dos dois.
- **Sem GC de blob**: a varredura está escrita (`BlobGC`) e nada a roda.

## Armadilhas do ambiente

- **`make` não existe** nesta máquina. Comandos crus.
- **`DATABASE_URL` precisa ficar VAZIO** durante `go test ./...`. `internal/lease`,
  `internal/janitor`, `internal/reaper`, `internal/scheduler` e `internal/api`
  pulam os testes SQL sem ela e a suíte passa; **setada e quebrada, o `TestMain`
  derruba o pacote inteiro**.
- **Asserções precisam de banco de rascunho.** Criar, migrar, rodar, derrubar —
  um banco com seed de demo mata as asserções em chave duplicada.
- Postgres de desenvolvimento: `postgres://farm@127.0.0.1:55432/...` (trust,
  `farm` é superusuário). Subir com `scripts\dev-up.ps1`.
- **Escolher porta livre para demo**: 8420 e 9090 costumam estar ocupadas, e o
  listener de métricas agora **falha o startup** numa porta tomada (de
  propósito). Usar `FARM_API_ADDR` e `FARM_METRICS_ADDR` juntos.
- `/.claude/` está no `.gitignore` — `gofmt -l .` da raiz voltou a ser confiável.
- Heredoc `python - <<'PY'` nesta máquina **come `\n` dentro de strings**
  ocasionalmente. Para editar arquivo, escrever o script com a ferramenta Write e
  chamá-lo, ou usar `sed`.

## Contexto que não está no código

Três pesquisas profundas estabeleceram, com fonte primária:

1. **Alugar aparelhos de provedores gerenciados está excluído por contrato**
   para uso contínuo e não-atendido de apps de terceiros. Sauce Labs AUP
   restringe a "legitimate testing or validation" e o ToS §1.1 faz da AUP
   condição da licença; AWS Service Terms §35.2 proíbe root e "install
   persistent software on devices"; BrowserStack §4.3 exige garantia de direitos
   sobre "the application package itself"; Kobiton §2.3 proíbe repassar acesso.
   Hardware próprio é o caminho.
2. **Supressão por gás limpo não detém evento de lítio.** Novec 1230 a 8,5 vol%
   falhou em suprimir **e** em impedir propagação; em nitrogênio puro, sem
   oxigênio e sem chama, propagou mesmo assim. Mitigação é contenção,
   espaçamento, limitação de carga e detecção precoce — as duas primeiras estão
   em `docs/siting.md`, a terceira espera o loop de política, a quarta chegou com
   o leitor de bateria.
3. **Código de incêndio não é o obstáculo.** IFC Tabela 1207.1.1 dispara em
   20 kWh; 60 aparelhos somam ~1 kWh. O obstáculo é política do operador de
   datacenter, e **isso não foi estabelecido em nenhuma direção** — é pergunta
   para resolver por escrito antes de comprar hardware.

Nunca verificado, e não deve ser afirmado como pronto: `internal/node` e
`internal/topo` **nunca rodaram contra hardware real** (são Linux por natureza —
o `farmd node` hoje sobe, bate heartbeat e para na descoberta USB), e `-race`
nunca rodou nesta máquina (não há compilador C).
