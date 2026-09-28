<!--
SYNC IMPACT REPORT (temporary — remove before committing this file)
- Version change: 1.0.0 -> 1.0.1
- Bump rationale: PATCH. The stack section's language floor was widened and two
  constraints were made explicit (FIFO grouping by wallet, multi-currency wallets).
  No principle was added, removed or redefined; no data or contract changes.
- Modified sections: "Stack tecnologico e restricoes arquiteturais"
  - "Go 1.22+" -> "Go 1.25 ou superior", with a rule that the floor tracks driver support.
  - Added: SQS MUST use FIFO queues with the message group derived from the wallet.
  - Added: multiple ISO 4217 currencies per wallet; no implicit conversion.
- Trigger: research for feature 001 found Go 1.22/1.23/1.24 are EOL (current stable
  1.27.1), pgx v5.11 requires go >= 1.25 and aws-sdk-go-v2 requires go >= 1.24.
  Holding the 1.22 floor would have frozen those dependencies on unpatched versions.
  Approved by the user as "raise floor to Go 1.25 + amend constitution".
- Modified principles: none.
- Added sections: none.
- Removed sections: none.
- Deferred items: none. No TODO(FIELD) placeholders retained.
-->

# Iron Ledger Constitution

## Core Principles

### I. Zero-Float Money (NON-NEGOTIABLE)

Dinheiro MUST NEVER ser representado, parseado, calculado, serializado ou persistido em
`float32`/`float64`. Toda quantia MUST usar um valor monetario exato: inteiro na menor unidade
monetaria (`int64` de centavos) acompanhado do codigo ISO 4217 da moeda, ou um decimal de
escala fixa com arredondamento explicito e testado.

- Parsing MUST rejeitar notacao exponencial e usar a precisao exata; `strconv.ParseFloat`,
  `fmt.Scan*` para dinheiro e coercoes implicitas via `float64` sao proibidos.
- Calculo MUST preservar precisao intermediaria (nunca truncar para "limpar" um valor).
- Serializacao MUST emitir o valor como inteiro de menor unidade ou string decimal exata, e
  MUST NOT usar JSON number para dinheiro.
- Persistencia MUST usar `NUMERIC`/inteiro no PostgreSQL; colunas monetarias nao podem ser
  `double precision` nem `real`.
- Divisao, juros, taxas e amortizacao MUST declarar a politica de arredondamento e o resto
  (rounding residual) de forma explicita no design.

**Rationale**: erro de ponto flutuante em saldo e silencioso, nao detectavel por testes
ordinarios e acumulativo. Nenhum valor monetario pode ser reconstruido a partir de um
float, mesmo que "quase correto".

### II. Append-Only Ledger

O ledger e imutavel: entradas ja contabilizadas MUST NEVER ser alteradas ou removidas.
`UPDATE` e `DELETE` sobre lancamentos MUST ser proibidos na aplicacao e no banco (revogacao
de `UPDATE`/`DELETE` no role da aplicacao e/ou trigger `RAISE EXCEPTION`).

- Correcoes MUST ser expressas como novos lancamentos (estorno/reversao e reemissao) que
  referenciam o lancamento original, preservando a trilha de auditoria.
- Todo lancamento MUST ser imutavel uma vez confirmado: sem edicao in-place de valor, moeda,
  data ou conta.
- O saldo MUST ser derivado da soma dos lancamentos (ou de um saldo materializado sempre
  reconciliavel com eles); o saldo nao e a fonte de verdade.
- Historico MUST ser suficiente para reproduzir qualquer saldo reportado em qualquer data.

**Rationale**: auditoria e rastreabilidade sao o produto. Um ledger mutavel torna
impossivel provar o saldo de um periodo passado e habilita fraude silenciosa.

### III. Invariantes Financeiras no Banco de Dados

Invariantes financeiras MUST ser garantidas pelo PostgreSQL (constraints, locks e
unicidade), nao apenas por validacao na aplicacao. A camada de aplicacao pode falhar
rapido, mas a ultima linha de defesa MUST estar no banco.

- Regras de saldo, invariantes de soma, domains de valor e relacoes MUST ser expressas como
  `CHECK`, `UNIQUE`, `FOREIGN KEY` ou `EXCLUDE` constraints.
- Concorrencia MUST ser resolvida com locking no nivel de linha (`SELECT ... FOR UPDATE`)
  ou isolamento `SERIALIZABLE`; validacao somente-aplicacao sob concorrencia e proibida.
- A aplicacao MUST NOT depender de uma constraint ausente: se a regra importa, a constraint
  existe.
- Constraints que nao podem ser `NOT NULL`/nao-`DEFERRABLE` devem ser declaradas
  `DEFERRABLE INITIALLY DEFERRED` quando a atomicidade exigir.
- Invariantes MUST ser verificadas por testes de integracao contra o PostgreSQL real
  (testcontainers ou equivalente), incluindo o teste de concorrencia correspondente.

**Rationale**: componentes concorrentes e retries tornam a verificacao na aplicacao
insuficiente por construcao. O banco e o unico ponto de serializacao confiavel.

### IV. Dominio Puro e Independente de Infraestrutura

O nucleo de dominio MUST NOT importar `fx` (Uber), `net/http`, SDKs de mensageria (SQS/AWS)
nem bibliotecas de persistencia (`pgx`, `database/sql`, ORMs). Dependencias de dominio
MUST ser injetadas como interfaces definidas no proprio dominio.

- Portas de entrada/saida MUST ser interfaces declaradas pelo dominio; adapters implementam.
- `fx` MUST ser confinado a `main`/camada de composicao e MUST NOT vazar para o dominio.
- Handlers HTTP, publishers SQS e repositorios MUST depender de casos de uso do dominio,
  nunca do contrario.
- O dominio MUST compilar e ser testavel sem Docker, sem rede e sem banco.

**Rationale**: o dominio e a parte que precisa ser auditada e provada correta; acopla-lo a
infraestrutura torna essa verificacao impossivel e a testabilidade cada vez mais cara.

### V. Publicacao de Eventos Somente Apos Commit

Eventos externos MUST ser publicados apenas depois do commit bem-sucedido da transacao que os
originou. Nenhum evento pode ser publicado dentro da transacao ou antes do commit.

- A ordem MUST ser: escrever o outbox dentro da mesma transacao de negocio -> commit ->
  publisher separado entrega.
- Mensagens MUST ser gravadas em outbox transacional no PostgreSQL na mesma transacao dos
  lancamentos; entrega e at-least-once, com retry e DLQ.
- Consumers MUST ser idempotentes (ver Principio VI); nao haExactly-once na fronteira.
- Como a deteccao de falha entre commit e publicacao e inevitavel, a reconciliacao MUST ser
  possivel: existe um caminho para reprocessar ou reprocessar o outbox.

**Rationale**: publicar antes do commit gera evento para lancamento que nunca existiu;
publicar sem outbox perde evento em queda entre commit e envio. O outbox elimina as duas
janelas.

### VI. Idempotencia Persistente

Idempotencia MUST ser persistida e MUST sobreviver ao reinicio de todos os processos.
Guardas em memoria, mutexes de processo e caches nao satisfazem este principio.

- Cada operacao idempotente MUST ter uma chave de idempotencia gravada no PostgreSQL com
  constraint `UNIQUE` (escopo minimo: operacao + chave, tipicamente + carteira/conta).
- Requisicao com chave repetida MUST NOT criar um segundo efeito e MUST NOT ser rejeitada como
  erro: deve retornar o resultado original.
- A gravacao da chave MUST ocorrer na mesma transacao do efeito, sob a mesma constraint.
- Consumers MUST deduplicar pela mensagem/evento, usando chave persistida.
- O teste MUST cobrir reinicio do processo entre a primeira tentativa e o retry.

**Rationale**: em sistemas com retry, rede e multiplas instancias, duplicidade e o modo de
falha mais comum. Idempotencia em memoria falha exatamente no cenario que mais importa.

### VII. Coordination Por Carteira, Sem Locks Globais

Locks globais sao proibidos. Nao existe mutex, advisory lock, singleton serializado ou
coordenacao que force o sistema inteiro a processar uma unidade por vez. A coordenacao MUST
ocorrer por carteira.

- O escopo de locking MUST ser a carteira/conta; duas carteiras MUST poder ser processadas
  concorrentemente sem coordenacao.
- Locks MUST ser derivados de linhas/`FOR UPDATE` por carteira (ou particionamento/sharding
  equivalente), nunca de um unico recurso compartilhado.
- Operacoes entre carteiras distintas MUST ser permitidas em paralelo; somente operacoes que
  tocam a mesma carteira contending por ela.
- Throughput MUST escalar com o numero de carteiras, nao ser limitado por um unico ponto de
  contencao.

**Rationale**: serializacao global vira o gargalo e o ponto unico de falha do sistema; o
ledger ja e naturalmente particionado por carteira, e a plataforma deve aproveitar isso.

## Stack tecnologico e restricoes arquiteturais

- **Linguagem**: Go 1.25 ou superior (MUST). O piso e a menor versao ainda com suporte da
  linguagem E compativel com os drivers obrigatorios (pgx, aws-sdk-go-v2); ele MUST ser
  revalidado a cada upgrade de driver e nunca MUST ser rebaixado para accommodate uma
  dependencia defasada.
- **Mensageria MUST usar filas FIFO**, com o grupo de mensagens derivado da carteira, para que a
  ordenacao por carteira seja garantida no broker e MUST NOT dependa de coordenaao global.
- **Dinheiro MUST suportar multiplas moedas** por carteira, com o codigo ISO 4217 persistido em
  toda quantia. Conversao entre moedas MUST NOT ser feita implicitamente em uma operacao: uma
  operacao so pode ocorrer na moeda da carteira, e qualquer conversao MUST ser uma operacao
  explicita, auditada e com taxa registrada.
- **Injecao de dependencia**: Uber Fx, restrito a `main` e a camada de composicao.
- **Persistencia**: PostgreSQL. Fonte de verdade e unico ponto de garantia das invariantes.
- **Mensageria**: AWS SQS, exercido contra LocalStack em desenvolvimento e testes.
- **Identidade**: Keycloak como IdP OAuth 2.0 / OIDC. A aplicacao MUST validar tokens
  emitidos pelo Keycloak (issuer, assinatura, `audience`, expiracao) e MUST NOT implementar
  fluxos de identidade proprios. Autorizacao por escopo/permissao MUST ser derivada das claims.
- **LocalStack MUST ser suficiente para o ciclo de desenvolvimento e de testes**; a aplicacao
  MUST rodar de forma equivalente contra AWS SQS real sem mudanca de codigo.

## Fluxo de desenvolvimento e quality gates

- Especificacao, plano e tarefas (`/speckit.*`) MUST ser derivados desta constituicao antes de
  qualquer implementacao.
- Toda alteracao que envolva dinheiro, invariantes, idempotencia ou publicacao de eventos
  MUST vir acompanhada de testes que demonstrem a invariante (unitarios no dominio,
  integracao contra PostgreSQL real e, quando houver concorrencia, teste com goroutines
  concorrentes).
- Schema migrations MUST ser versionadas e aplicadas de forma idempotente; alteracao de
  schema MUST revisar as constraints dos principios III, VI e VII.
- Codigo que viole um principio NON-NEGOTIABLE MUST ser rejeitado em review; nao existe
  excecao silenciosa. Desvios exigem emenda previa desta constituicao.
- Complexidade adicional MUST ser justificada no plano antes de ser implementada (YAGNI).

## Governance

Esta constituicao prevalece sobre qualquer outra pratica, convencao de codigo ou preferencia
de equipe. Em caso de conflito, esta constituicao vence.

**Emendas**:

1. Proposta escrita com a motivacao, os principios afetados e o plano de migracao.
2. Aprovacao explicita do maintainer responsavel pelo sistema financeiro.
3. Atualizacao de `Version`, `Last Amended` e do relatorio de impacto de sincronizacao.
4. Comunicacao a equipe e, quando oprincipio alterar dados ja gravados, plano de migracao
   de dados executado antes de producao.

**Politica de versionamento** (semantica):

- **MAJOR**: remocao ou redefinicao incompativel de um principio, ou mudanca que invalide
  dados/contratos existentes.
- **MINOR**: adicao de principio ou secao, ou expansao material de regra existente.
- **PATCH**: clarificacao de redacao, correcao de typo, refino nao semantico.

**Revisao de conformidade**:

- Todo PR e toda revisao MUST verificar conformidade com os sete principios.
- O plano de cada feature MUST declarar explicitamente como cada principio aplicavel e
  respeitado (ou por que nao se aplica).
- Revisao de conformidade MUST ocorrer antes de mergers que toquem `internal/domain`,
  schema do banco, publisher de eventos ou limites de transacao.
- Divergencia entre codigo e constituicao MUST ser tratada primeiro como bug no codigo,
  nunca como excecao na regra.

**Version**: 1.0.1 | **Ratified**: 2026-09-28 | **Last Amended**: 2026-09-28
