# Estado da sessão — retomar daqui

Atualizado em 2026-09-05, depois de mergear as trinta branches abertas e
reverificar o registro contra a árvore resultante. Este arquivo é o único lugar
que reúne o estado; leia-o antes de qualquer coisa.

## Onde o repositório está

- `main` = `82ecdbc`, **empurrado para o origin**. Árvore limpa.
- Remote: `https://github.com/FlavioNeto11/device-farmer`
- **Nenhum PR aberto.** Trinta mergeados no total; os vinte e um desta rodada
  foram `--no-ff`, então o histórico mostra o que cada um trouxe.
- Schema **v17**, contígua (`00001`…`00017`). 184 arquivos Go, **1 575 testes de
  topo em 21 pacotes**, **11 suítes de asserção SQL**.
- Nove papéis: `api`, `scheduler`, `reaper`, `recovery`, `jobrunner`, `janitor`,
  `chargepolicy`, `watchdog`, `node` — mais `all` e `demo`, que multiplexam.

### Verificado de ponta a ponta nesta sessão

```
go build ./... && go vet ./... && gofmt -l .      # limpos
go test -count=1 ./...                            # 21 pacotes ok
farmd migrate up  (banco vazio → v17)             # 17 migrations
test/assertions*.sql                              # 11 suítes, todas PASSED
```

Contra um farm vivo (56 devices simulados, schema v17): `healthz` 200,
`/api/v1/capabilities` **401 sem credencial e 200 com**, todas as rotas de
leitura 200 para operator, **`/api/v1/reaper` e `/api/v1/bulk` 403 para
tenant**, as 7 áreas do Docs 200 com as contagens batendo, `/metrics` 200 com
1 031 linhas e todas as séries que alguma regra de alerta nomeia presentes.

Três provas vivas que valem mais que a suíte:

1. **A testemunha escreve.** Job de 100 s com `FARM_LEASE_WITNESS_INTERVAL=30s`:
   lease colocado às 17:19:08, `farm.leases.witness_at` gravado às 17:19:38 com
   `witness_extensions=1` e `reclaimable_at` empurrado para 18:04:08;
   `farm_jobrunner_witness_total{outcome="accepted"} 1`. Era `LEASE-09`, a linha
   mais importante do registro, e era a última metade não ligada do
   contramedida do #663.
2. **O reaper se recusa a armar num componente que nunca bateu.**
   `farm.reaper_arm(ARRAY['reaper','api','ghost_component'], '60s')` devolve
   `f | 00:00:00 | {ghost_component}` e `reaper_state.last_refusal` diz por quê.
3. **Fechar quarentena termina na própria chamada.** Quarentena de hub sobre 7
   devices: `devices_released: 7`, `devices_reenabled: 7`, e o banco lê
   `unknown|enabled|7` no instante seguinte — sem ciclo de recovery no meio.

## O que mudou nesta rodada

Vinte e uma branches, na ordem registrada em `<scratchpad>/merge-order.md`, com
`go build && go vet && gofmt -l && go test` limpo entre cada uma. Sete
precisaram de reconciliação de verdade, e essas são as que importam:

- **`internal/recovery`: dois classificadores viraram um.** #19 deu ao
  `classifyHostFault` do atuador um terceiro retorno (o *kind* da recusa) e #24
  extraiu a MESMA decisão para um `ClassifyHostFault` exportado, para a rota de
  slot power do operador escrever linhas indistinguíveis das da escada.
  Mergeados, sobravam duas implementações de uma decisão. `HostFault` carrega
  `RefusalKind`, e o método do atuador é aquela função com a identidade do
  degrau amarrada.
- **`config.Fence` colidiu.** #18 é o proxy que o *host* SERVE
  (`FARM_FENCE_TLS_*`); #21 é o que um processo do plano de controle
  APRESENTA (`FARM_FENCE_CLIENT_*`). Os dois se chamavam `Fence` e um sombreava
  o outro sem o compilador reclamar. O lado cliente virou `Config.FenceClient`.
- **`deviceLease.ID/Holder/JobID` viraram ponteiros** (#12, mascaramento por
  tenant) e quatro superfícies novas de `ctl` formatavam com `%s`. `go vet`
  pegou todas.
- **A linha do marcador no `Summary()`** que #23 adicionou foi perdida numa
  resolução de conflito dois merges depois e restaurada — o teste do próprio #23
  a encontrou.
- **`assertions_v15.sql`** arma o reaper pela assinatura nova do `00012`.
- **`TestEveryTenantReadableRouteIsScoped`** (#12) reprovou o build por causa do
  `GET /api/v1/slots` que #16 tinha acabado de adicionar. É o único tipo de
  evidência que um teste desses consegue oferecer, e o resultado foi uma entrada
  na allowlist com a razão escrita: `slotView` não carrega lease, job nem tenant.

Um defeito real apareceu na verificação final e foi corrigido:

- **`internal/demo` roubava jobs submetidos pela API.** `runDemo` sobe o
  scheduler e o jobrunner REAIS ao lado do simulador, e o `schedulableJobs` do
  simulador pegava todo job na fila. O modelo dele de duração é a CONTAGEM de
  steps, então ele soltava o lease em quatro segundos enquanto o runner estava a
  um segundo de cem, e o step do operador ficava `running` para sempre sob uma
  linha de log dizendo "job complete". Agora o simulador só agenda o que ele
  mesmo enfileirou (`farm.jobs.created_by = 'demo-feeder'`). Dois testes novos,
  ambos falsificados.

## Estado do registro

`REQUIREMENTS.md` e a aba **Docs → Requirements** estão sincronizados
célula-a-célula (68 linhas divergiam; agora o JSON é gerado do Markdown).

- **69 de 101** linhas em `met`, mais **8** em `met` numa dimensão e abertas em
  outra, mais **3** `decided`.
- **21 abertas**, e a separação delas é a informação:
  - **precisam de rack** (6): `DEV-04`, `DEV-05`, `REC-03`, `OPS-04`, `HW-05`,
    `JOB-04`. Nada nesta árvore jamais encostou num aparelho.
  - **são relato** (9): `LEASE-14`, `JOB-08`, `JOB-10`, `API-06`, `API-07`,
    `SEC-03`, `OBS-10`, `TEST-02`, `TEST-03`. Dá para descobrir o que aconteceu,
    às vezes só pelo `psql`.
  - **nomeadas e deliberadamente não feitas** (6): `LEASE-10`, `LEASE-13`,
    `DEV-02`, `DEV-07`, `JOB-03`, `SEC-05`.
- Os gaps das seis áreas caíram de **88 para 20**. Vinte e nove foram movidos
  para a tabela "Gaps closed" de cada página — registrados, não apagados, porque
  quem lembra de um gap precisa distinguir **corrigido** de **nunca existiu**.

## O que fazer ao retomar, nesta ordem

1. **Rodar contra um rack.** É o item 1 do registro e seis linhas esperam por
   ele. `farmd node` lê `/sys` e para na descoberta USB fora do Linux; reset USB,
   corte de VBUS por `uhubctl` e a metade-host do proxy de fence estão escritos e
   testados contra `fstest.MapFS` e `test/fakeadb`, e nada disso é um telefone.
2. **`JOB-08`** — mostrar qual step falhou e o que ele imprimiu. As linhas estão
   em `farm.job_steps`; falta a rota e o painel.
3. **`JOB-10`** — reconciliar o estado do job com as linhas de step. O caso que
   produzia a divergência no demo foi corrigido; a reconciliação que a tornaria
   impossível não existe.
4. O resto está ordenado em `REQUIREMENTS.md` → *What the register argues for
   next*.

## Defeitos conhecidos e ainda abertos em `main`

- **Nunca rodou contra hardware.** Não afirmar o contrário em lugar nenhum.
- **`-race` nunca rodou nesta máquina** (não há compilador C).
- **`.env` é versionado** (veio com o `--profile` do compose) e o `.gitignore`
  não o cobre. Não tem segredo nenhum hoje e o comentário no topo diz que o
  farmd nunca o lê, mas é o arquivo onde alguém vai colar um `DATABASE_URL` com
  senha.
- Uma mensagem de commit (`a832121`) perdeu a palavra "armed" para uma expansão
  de crase do shell. O conteúdo está certo; a frase ficou com um buraco.

## Armadilhas do ambiente

- **`make` não existe** nesta máquina. Comandos crus.
- **`DATABASE_URL` precisa ficar VAZIO** durante `go test ./...`. Os pacotes com
  teste SQL pulam sem ela e a suíte passa; **setada e quebrada, o `TestMain`
  derruba o pacote inteiro**.
- **Asserções precisam de banco de rascunho.** Criar, migrar, rodar, derrubar —
  um banco com seed de demo mata as asserções em chave duplicada.
- Postgres de desenvolvimento: `postgres://farm@127.0.0.1:55432/...` (trust,
  `farm` é superusuário — asserções de GRANT têm que passar por `SET ROLE`).
  Subir com `scripts\dev-up.ps1`.
- **Escolher porta livre para demo**: 8420 e 9090 costumam estar ocupadas.
  Usar `FARM_API_ADDR` e `FARM_METRICS_ADDR` juntos. Um listener de métricas que
  não consegue fazer bind **não derruba mais o papel** — ele loga e exporta
  `farm_metrics_listener_up 0`.
- `/.claude/` está no `.gitignore` — `gofmt -l .` da raiz é confiável.
- Heredoc `python - <<'PY'` nesta máquina **come `\n` dentro de strings**
  ocasionalmente, e crases dentro de heredoc `<<'MSG'` do `git commit -F -`
  **são expandidas pelo shell**. Para editar arquivo, escrever o script com a
  ferramenta Write e chamá-lo, ou usar `sed`.
- `farm.jobs` exige tenant e queue que existam: no demo são `acme` e `ci`, não
  `default`. O pool é `default`. Um spec precisa de `"version": 1` e o payload
  vai numa chave com o nome do kind (`{"kind":"sleep","sleep":{"duration":"..."}}`).

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
   em `docs/siting.md`, e as duas últimas chegaram nesta rodada
   (`internal/chargepolicy` segura a banda 40–80%, `internal/watchdog/swell.go`
   levanta `battery_anomaly` com `rack_slot`, e `DeviceFarmerBatteryAnomaly`
   pagina em cima).
3. **Código de incêndio não é o obstáculo.** IFC Tabela 1207.1.1 dispara em
   20 kWh; 60 aparelhos somam ~1 kWh. O obstáculo é política do operador de
   datacenter, e **isso não foi estabelecido em nenhuma direção** — é pergunta
   para resolver por escrito antes de comprar hardware.

O invariante, para quem chegar sem contexto: **um lease termina quando o job
diz, quando um prazo que o usuário escreveu vence, ou quando um humano o toma de
volta. Nada mais.** Há testes que reprovam o build se vocabulário de alocação
aparecer em `internal/adbwire`, em `internal/recovery/adbactuator.go` ou em
`internal/fenceproxy`. Leia o teste do pacote antes de escrever nele.
