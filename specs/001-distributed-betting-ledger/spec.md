# Feature Specification: Distributed Betting Ledger

**Feature Branch**: `001-distributed-betting-ledger`

**Created**: 2026-09-28

**Status**: Draft

**Input**: User description: "Implementar um serviço backend em Go para processamento distribuído de apostas. O sistema deve expor uma API HTTP e um consumidor de mensagens (SQS) que movimentem carteiras de jogadores com garantias equivalentes. As operações financeiras externas suportadas são BET, WIN, LOSS, REFUND e ROLLBACK. O resultado financeiro deve permanecer correto com múltiplas instâncias em execução e falhas entre as etapas do processamento, garantindo idempotência persistente, integridade do ledger e ausência de saldo negativo."

## User Scenarios & Testing *(mandatory)*

<!--
  IMPORTANT: User stories should be PRIORITIZED as user journeys ordered by importance.
  Each user story/journey must be INDEPENDENTLY TESTABLE - meaning if you implement just ONE of them,
  you should still have a viable MVP (Minimum Viable Product) that delivers value.

  Assign priorities (P1, P2, P3, etc.) to each story, where P1 is the most critical.
  Think of each user story as a standalone slice of functionality that can be:
  - Developed independently
  - Tested independently
  - Deployed independently
  - Demonstrated to users independently
-->

### User Story 1 - Registrar aposta e debitar a carteira (Priority: P1)

Como operador de apostas, quero registrar uma aposta (BET) para um jogador e ver o valor
debitado da carteira do jogador, para que a aposta possa ser aceita somente quando houver
fundos disponíveis e o resultado financeiro da operação seja imediatamente visível para o
jogador.

**Why this priority**: BET é a operação que inicia qualquer ciclo de vida de aposta e é a
única capaz de produzir valor (saldo de jogador) a partir do zero. Sem ela nenhuma das
demais operações faz sentido, e é o slice mínimo que já entrega valor verificável.

**Independent Test**: Pode ser testado integralmente enviando uma única operação BET para uma
carteira recém-criada com saldo conhecido e conferindo o saldo resultante e o lançamento
gerado, sem depender de WIN, LOSS, REFUND ou ROLLBACK.

**Acceptance Scenarios**:

1. **Given** uma carteira de jogador com saldo de R$ 100,00 e uma aposta de R$ 30,00 sem
   referência repetida, **When** a operação BET é submetida pela API, **Then** a resposta
   confirma a aposta e o saldo da carteira passa a R$ 70,00.
2. **Given** uma carteira com saldo de R$ 20,00 e uma aposta de R$ 30,00, **When** a
   operação BET é submetida, **Then** a operação é recusada, nenhum lançamento é gravado e o
   saldo permanece R$ 20,00.
3. **Given** uma operação BET cuja referência já foi processada anteriormente, **When** a
   mesma operação é submetida novamente com a mesma chave, **Then** o sistema devolve o
   resultado original e nenhum lançamento adicional é criado.
4. **Given** uma aposta BET registrada, **When** o jogador consulta seu saldo, **Then** o
   valor apresentado corresponde à soma de todos os lançamentos da carteira.

---

### User Story 2 - Liquidar o resultado da aposta (Priority: P2)

Como operador de apostas, quero liquidar uma aposta como WIN ou LOSS para creditar o
prêmio ao jogador ou encerrar a perda, para que o resultado da aposta esteja refletido no
saldo do jogador com rastreabilidade completa até a aposta original.

**Why this priority**: Sem liquidação o sistema não completa o ciclo de vida da aposta e não
entrega o valor central ao usuário final. Depende apenas do P1 estar disponível.

**Independent Test**: Pode ser testado registrando uma aposta (BET) via API e, em seguida,
submetendo um WIN e um LOSS para apostas distintas, conferindo os saldos resultantes e as
referências entre os lançamentos.

**Acceptance Scenarios**:

1. **Given** uma aposta BET de R$ 30,00 liquidada como WIN de prêmio R$ 90,00, **When** a
   operação WIN é submetida, **Then** o saldo do jogador aumenta em R$ 90,00 e a operação
   referencia a aposta original.
2. **Given** uma aposta BET de R$ 30,00 liquidada como LOSS, **When** a operação LOSS é
   submetida, **Then** nenhum valor é creditado ou debitado do jogador e a aposta passa a
   constar como perdida no histórico.
3. **Given** uma aposta já liquidada, **When** uma segunda operação de liquidação com
   referência diferente é submetida para a mesma aposta, **Then** a operação é recusada e o
   saldo permanece inalterado.
4. **Given** uma operação WIN repetida com a mesma chave, **When** é reenviada após uma
   falha de comunicação, **Then** o prêmio é creditado uma única vez.

---

### User Story 3 - Corrigir uma operação já registrada (Priority: P3)

Como operador de apostas, preciso estornar ou reverter uma operação por meio de REFUND e
ROLLBACK, para que erros de processamento e apostas canceladas sejam corrigidos sem apagar o
histórico e sem que a correção afete lançamentos já existentes.

**Why this priority**: Correção é essencial para operação real (erro do provedor de jogos,
aposta cancelada, suspensa), mas o sistema já é útil e auditável sem ela; entra logo após o
ciclo principal.

**Independent Test**: Pode ser testado registrando uma aposta via API, aplicando um REFUND e um
ROLLBACK sobre operações distintas e verificando que os lançamentos originais permanecem
intactos, que existem lançamentos de correção e que o saldo reflete exatamente essas
correções.

**Acceptance Scenarios**:

1. **Given** uma aposta BET de R$ 30,00 em uma carteira com saldo de R$ 70,00, **When** um
   REFUND de R$ 30,00 é submetido, **Then** o saldo volta a R$ 100,00 e o lançamento original
   de BET permanece inalterado e visível no histórico.
2. **Given** um REFUND já aplicado a uma operação, **When** o mesmo REFUND é submetido
   novamente com a mesma chave, **Then** o jogador não recebe o valor duas vezes.
3. **Given** uma correção que resultaria em saldo negativo do jogador, **When** a operação é
   submetida, **Then** a operação é recusada e o saldo permanece inalterado.
4. **Given** qualquer operação já registrada, **When** um responsável tenta alterá-la ou
   removê-la, **Then** a alteração é impedida e a tentativa é registrada para auditoria.

---

### User Story 4 - Processar operações pela fila de mensagens com as mesmas garantias (Priority: P4)

Como integrador do ecossistema de apostas, quero enviar operações financeiras pela fila de
mensagens (SQS) e ter o mesmo resultado financeiro, as mesmas validações e as mesmas
garantias que pela API, para que eu possa integrar o serviço a sistemas de apostas de terceiros
sem duplicar regras de negócio.

**Why this priority**: A equivalência entre os dois canais é o que torna o serviço utilizável
em produção por múltiplos provedores; porém ela só pode ser construída sobre os casos de uso
financeiros já entregues por P1–P3.

**Independent Test**: Pode ser testado publicando uma operação BET na fila, uma WIN e um
REFUND em mensagens separadas, e verificando que os saldos resultantes são idênticos aos
obtidos pelas mesmas operações submetidas pela API em outra carteira.

**Acceptance Scenarios**:

1. **Given** uma mensagem BET válida na fila, **When** ela é consumida, **Then** a carteira do
   jogador é debitada e a resposta é equivalente à operação BET equivalente pela API.
2. **Given** uma mensagem com saldo insuficiente, **When** ela é consumida, **Then** nenhum
   lançamento é gravado e a mensagem é tratada como falha registrada, sem nova tentativa
   cega do mesmo efeito.
3. **Given** o mesmo evento publicado mais de uma vez na fila (redelivery), **When** as
   cópias são consumidas, **Then** o efeito financeiro ocorre exatamente uma vez.
4. **Given** uma falha depois que a operação financeira foi registrada e antes da conclusão
   do processamento da mensagem, **When** o sistema é reiniciado, **Then** a operação não é
   reaplicada e nenhum estado intermediário fica pendente de forma inconsistente.
5. **Given** uma mensagem inválida ou corrompida, **When** ela é consumida, **Then** ela é
   isolada para tratamento posterior e não bloqueia o processamento de mensagens seguintes.
6. **Given** uma mensagem WIN cujo BET correspondente ainda não foi processado, **When** ela é
   consumida, **Then** ela é reprocessada com espera progressiva e, se a aposta continuar
   ausente ao fim das tentativas, a recusa é registrada com identificação da mensagem e do
   motivo.
7. **Given** uma carteira em USD e uma operação BET em BRL, **When** ela é consumida, **Then**
   a operação é recusada, nenhum lançamento é gravado e a moeda divergente é informada no motivo.

---

### User Story 5 - Manter o saldo correto com múltiplas instâncias e falhas (Priority: P5)

Como responsável pela operação da plataforma, quero que o saldo dos jogadores permaneça
exatamente correto mesmo com várias instâncias do serviço executando ao mesmo tempo e com
falhas entre as etapas do processamento, para que eu possa escalar o serviço e nunca
precisar corrigir saldos manualmente.

**Why this priority**: É a garantia que sustenta todos os demais benefícios e o motivo de
existir do serviço; porém é uma propriedade transversal, verificada por testes que também
cobrem P1–P4.

**Independent Test**: Pode ser testado enviando simultaneamente muitas operações concorrentes
sobre a mesma carteira, a partir de múltiplas instâncias, interrompendo e reiniciando o
processo durante o processamento e verificando ao final que o saldo é exatamente igual ao
calculado a partir dos lançamentos válidos.

**Acceptance Scenarios**:

1. **Given** uma carteira com saldo de R$ 100,00, **When** duas operações BET de R$ 80,00 são
   submetidas simultaneamente, **Then** apenas uma é aceita, o saldo final é R$ 20,00 e não
   há saldo negativo em momento algum.
2. **Given** operações concorrentes sobre carteiras de jogadores distintos, **When** são
   processadas, **Then** o throughput do serviço não é limitado a uma única operação por vez
   para todo o sistema.
3. **Given** duas instâncias do serviço recebendo a mesma operação com a mesma chave, **When**
   ambas a processam, **Then** o efeito financeiro ocorre exatamente uma vez.
4. **Given** uma falha abrupta após a movimentação financeira e antes da confirmação ao
   solicitante, **When** o solicitante repete a operação com a mesma chave, **Then** ele obtém
   o resultado original, sem duplicidade.
5. **Given** uma carteira com saldo de R$ 50,00, **When** qualquer combinação de operações
   válidos e inválidas é submetida, **Then** o saldo permanece sempre maior ou igual a zero em
   todas as leituras.

---

### User Story 6 - Auditar e reconciliar o histórico financeiro (Priority: P6)

Como auditor, quero consultar o histórico completo de movimentações de cada carteira e
conferir que o saldo atual é a soma de todos os lançamentos válidos, para que divergências
possam ser detectadas e investigadas antes de afetarem jogadores ou a operação.

**Why this priority**: É pré-requisito de conformidade e suporte, mas não é bloqueia a entrega
das operações financeiras; entra por último.

**Independent Test**: Pode ser testado após qualquer sequência de operações: consultar o
histórico de uma carteira, somar os lançamentos manualmente e comparar com o saldo informado,
incluindo uma carteira com operações contestadas.

**Acceptance Scenarios**:

1. **Given** uma carteira com múltiplas operações, **When** o histórico é consultado, **Then**
   cada lançamento exibe operação, valor, moeda, data e a relação com a operação que originou
   ou corrigiu o lançamento.
2. **Given** um histórico de carteira, **When** a soma dos lançamentos válidos é calculada,
   **Then** o resultado é igual ao saldo atual informado.
3. **Given** uma operação rejeitada por saldo insuficiente, **When** o histórico é consultado,
   **Then** existe registro da tentativa rejeitada com o motivo, sem alteração de saldo.
4. **Given** uma divergência detectada na reconciliação, **When** a verificação é executada,
   **Then** a divergência é reportada com identificação da carteira e do período afetado.

---

### Edge Cases

<!--
  ACTION REQUIRED: The content in this section represents placeholders.
  Fill them out with the right edge cases.
-->

- Valor zero em qualquer operação: BET, WIN ou REFUND de valor zero é aceito como operação
  sem efeito financeiro ou recusado, de forma uniforme para todos os canais.
- Valor negativo na entrada: uma operação com valor negativo ou não positivo é sempre
  recusada, nunca reinterpretada.
- Precisão de centavo e house money: operações com mais casas decimais que a moeda suporta são
  recusadas, nunca arredondadas silenciosamente.
- Apostas de múltiplas moedas: a plataforma suporta carteiras em BRL e em USD. Uma operação cuja
  moeda difere da moeda da carteira é sempre recusada; não há conversão implícita. Uma conversão
  de moeda, quando existir no futuro, será uma operação explícita, auditada e com a taxa
  aplicada registrada no momento da operação.
- Operação duplicada com chave diferente: reenvio do mesmo negócio com chave de idempotência
  diferente é tratado como operação nova e registrado como possível duplicidade para
  auditoria.
- Aposta sem saldo suficiente em processamento concorrente: a segunda operação concorrente é
  recusada sem violar o saldo, sem saldo negativo e sem lançamento parcial.
- Falha entre a gravação do lançamento e a resposta ao solicitante: o solicitante reenvia com
  a mesma chave e obtém o resultado original.
- Falha após o envio do evento externo mas antes da confirmação: o evento é reentregue sem
  duplicar o efeito financeiro no consumidor.
- ROLLBACK de operação já liquidada: ROLLBACK reverte a operação alvo criando lançamentos
  compensatórios, inclusive quando a aposta já está liquidada como WIN ou LOSS. O lançamento
  original permanece intacto. Se a reversão exigir saldo negativo, a operação é recusada com
  motivo explícito e o solicitante deve repor fundos antes de repetir.
- Operação referenciando aposta inexistente: WIN, LOSS, REFUND ou ROLLBACK cuja aposta ainda não
  foi registrada é tratada como falha transitória. A mensagem é reprocessada com espera
  progressiva por um número limitado de tentativas; se a aposta ainda não existir ao fim das
  tentativas, a mensagem é isolada para tratamento posterior e a recusa fica registrada com
  identificação da mensagem e do motivo.
- Carteira inexistente ou inativa: operação é recusada sem criar carteira implicitamente.
- Retry após falha definitiva: mensagem que falha de forma permanente não entra em laço
  infinito de reprocessamento.
- Crescimento da fila: atraso no consumo não pode ser confundido com perda de operação
  financeira já registrada.
- Concorrência sobre a mesma carteira versus sobre carteiras distintas: operações de carteiras
  distintas não podem se bloquear mutuamente.

## Requirements *(mandatory)*

<!--
  ACTION REQUIRED: The content in this section represents placeholders.
  Fill them out with the right functional requirements.
-->

### Functional Requirements

- **FR-001**: O sistema MUST expor uma API HTTP para as operações BET, WIN, LOSS, REFUND e
  ROLLBACK.
- **FR-002**: O sistema MUST consumir as mesmas cinco operações por meio de um consumidor de
  mensagens da fila (SQS).
- **FR-003**: O sistema MUST produzir resultado financeiro idêntico para uma operação
  submetida pela API e para o mesmo evento publicado na fila.
- **FR-004**: Cada operação externa MUST aceitar uma chave de idempotência obrigatória, e o
  sistema MUST devolver o resultado original ao receber a mesma chave novamente, sem criar um
  segundo efeito financeiro.
- **FR-005**: A idempotência MUST ser persistente e MUST continuar válida após o reinício de
  todas as instâncias do serviço.
- **FR-006**: O saldo de uma carteira MUST ser sempre maior ou igual a zero; qualquer operação
  que resulte em saldo negativo MUST ser recusada integralmente, sem lançamento parcial.
- **FR-007**: Todas as movimentações MUST ser registradas como lançamentos em um histórico
  somente de acréscimo; lançamentos registrados MUST NOT ser alterados ou removidos, e
  correções MUST ser registradas como novas movimentações que referenciam a original.
- **FR-008**: O saldo corrente MUST ser sempre reconciliável com a soma dos lançamentos válidos
  da carteira.
- **FR-009**: O sistema MUST garantir a correção do saldo com múltiplas instâncias executando
  simultaneamente, inclusive quando operações concorrentes atingem a mesma carteira.
- **FR-010**: O sistema MUST concluir a movimentação financeira e a marcação de processamento
  concluído de forma que uma falha entre essas etapas não resulte em efeito duplicado nem em
  operação perdida.
- **FR-011**: Toda operação MUST ser gravada com identificação da operação, tipo, carteira,
  valor na menor unidade monetária, moeda, data e a relação com a operação de origem ou de
  correção quando aplicável.
- **FR-012**: O sistema MUST NOT usar ponto flutuante em nenhuma etapa de recebimento,
  cálculo, armazenamento, transmission ou apresentação de valores monetários.
- **FR-013**: Valores MUST ser transmitidos e persistidos como inteiro na menor unidade
  monetária acompanhado do código de moeda, e operações com precisão acima da suportada pela
  moeda MUST ser recusadas sem arredondamento.
- **FR-014**: O sistema MUST registrar saldo e moeda por carteira, MUST suportar carteiras em BRL
  e em USD e MUST recusar operações cuja moeda difere da moeda da carteira, sem realizar
  conversão implícita.
- **FR-015**: BET MUST debitar a carteira do jogador e MUST ser aceita somente se o saldo for
  suficiente no momento da aplicação.
- **FR-016**: WIN MUST creditar o prêmio ao jogador e MUST referenciar a aposta liquidada.
- **FR-017**: LOSS MUST encerrar a aposta sem mover valores da carteira do jogador.
- **FR-018**: REFUND MUST devolver ao jogador o valor de um débito anterior, sem alterar o
  lançamento original.
- **FR-019**: ROLLBACK MUST reverter a operação alvo criando lançamentos compensatórios que
  referenciam a operação original, inclusive quando a aposta já está liquidada, e MUST ser
  recusada com motivo explícito quando a reversão exigir saldo negativo.
- **FR-020**: Uma aposta MUST NOT ser liquidada mais de uma vez; liquidações subsequentes MUST
  ser recusadas.
- **FR-021**: Uma operação MUST referenciar uma aposta existente; WIN, LOSS, REFUND ou ROLLBACK
  cuja aposta ainda não está registrada MUST ser tratada como falha transitória, reprocessada
  com espera progressiva por número limitado de tentativas e, se persistir, isolada para
  tratamento posterior com recusa registrada.
- **FR-021a**: O processamento de mensagens MUST preservar a ordem das operações que atingem a
  mesma carteira por meio do agrupamento da fila por carteira, de modo que operações sobre
  carteiras distintas sejam processadas em paralelo.
- **FR-022**: O consumidor de mensagens MUST tratar redelivery da mesma mensagem sem duplicar o
  efeito financeiro.
- **FR-023**: O consumidor MUST separar mensagens com falha permanente das mensagens
  reprocessáveis, e mensagens com falha permanente MUST NOT bloquear o processamento de
  mensagens subsequentes.
- **FR-024**: O consumidor MUST NOT manter, em memória do processo, a única cópia do estado que
  garante a idempotência ou a integridade financeira.
- **FR-025**: O sistema MUST garantir as invariantes financeiras de forma independente de
  quantas instâncias estejam ativas, inclusive com falha abrupta no meio do processamento.
- **FR-026**: O sistema MUST permitir consultar o histórico completo de movimentações de uma
  carteira, incluindo operações rejeitadas com seu motivo.
- **FR-027**: O sistema MUST disponibilizar verificação de reconciliação entre o saldo corrente
  e a soma dos lançamentos válidos, reportando divergências com identificação de carteira e
  período.
- **FR-028**: O sistema MUST autenticar e autorizar o solicitante, validando identidade e
  permissões antes de executar qualquer operação financeira, e MUST derivar a autorização das
  declarações do provedor de identidade da plataforma.
- **FR-029**: O sistema MUST validar que toda requisição e mensagem pertence a um tenant
  identificado e MUST impedir que uma instância ou um tenant leia ou movimente dados de outro.
- **FR-030**: O sistema MUST registrar para auditoria quem originou cada operação, quando, por
  qual canal e com qual chave de idempotência.
- **FR-031**: O sistema MUST reportar ao solicitante o motivo da recusa de forma estável e
  distinguível (saldo insuficiente, operação duplicada, referência inexistente, valor ou moeda
  inválida, não autorizado).
- **FR-032**: O sistema MUST expor verificação de saúde do serviço e de suas dependências
  imediatamente após a inicialização, para que a recusa de tráfego em caso de falha seja
  automática.
- **FR-033**: O sistema MUST escalar horizontalmente sem coordenação global, permitindo que
  operações sobre carteiras distintas sejam processadas em paralelo.
- **FR-034**: O sistema MUST preservar a ordem de aplicação das operações concorrentes sobre a
  mesma carteira, de modo que o resultado não dependa da ordem arbitrária de chegada.
- **FR-035**: O sistema MUST emitir eventos de domínio para outros sistemas somente depois que a
  movimentação financeira correspondente estiver confirmada de forma durável.
- **FR-036**: O sistema MUST permitir reenviar, após falha ou reinício, os eventos que não
  foram entregues aos destinatários, sem exigir reenvio manual.

### Key Entities *(include if feature involves data)*

- **Player Wallet (Carteira do Jogador)**: Saldo disponível de um jogador, em uma única moeda,
  com identificador do jogador e do tenant. Nunca fica negativo. É a unidade de saldo
  consultada pelo jogador.
- **Ledger Entry (Lançamento)**: Movimentação individual e imutável que compõe o histórico de
  uma carteira. Registra tipo de operação, valor na menor unidade, moeda, instante, carteira,
  origem e a referência a lançamentos relacionados. Nunca é editado ou removido.
- **Operation (Operação)**: Intenção de negócio recebida por um dos canais (API ou fila),
  identificada por chave de idempotência, contendo tipo (BET, WIN, LOSS, REFUND, ROLLBACK),
  carteira, valor, moeda e referência externa. É a unidade de idempotência e de auditoria.
- **Bet (Aposta)**: Aposta registrada por uma operação BET e seu estado de liquidação
  (registrada, ganha, perdida, estornada, revertida). Referencia a carteira e a operação que a
  criou.
- **Balance (Saldo)**: Valor corrente derivado da carteira, sempre reconciliável com a soma
  dos lançamentos válidos da carteira.
- **Idempotency Record (Registro de Idempotência)**: Associação durável entre chave de
  idempotência e operação já processada, contendo o resultado devolvido para reapresentação.
- **Audit Record (Registro de Auditoria)**: Evidência imutável de quem executou cada operação,
  por qual canal, quando e com qual resultado.

### Success Criteria *(mandatory)*

<!--
  ACTION REQUIRED: The content in this section represents placeholders.
  Fill them out with the right functional requirements.
-->

### Measurable Outcomes

- **SC-001**: 100% das operações financeiras aceitas produzem exatamente um lançamento no
  histórico, verificado por reconciliação automática diária sem divergências.
- **SC-002**: Nenhum saldo de carteira é observado como negativo em nenhum momento, sob carga
  com operações concorrentes e falhas induzidas.
- **SC-003**: Ao repetir qualquer operação com a mesma chave de idempotência, 100% dos casos
  resultam em um único efeito financeiro, inclusive após reinício completo do serviço.
- **SC-004**: O saldo reportado após qualquer sequência aleatória de operações, com falhas e
  reinícios, é idêntico ao saldo calculado pela soma dos lançamentos válidos.
- **SC-005**: Uma operação idempotente repetida devolve o mesmo resultado original sem alterar
  o saldo, e a operação repetida pela API e pela fila produz o mesmo efeito que a operação
  original.
- **SC-006**: A partir de 10.000 operações concorrentes sobre a mesma carteira, o saldo final
  corresponde exatamente ao valor esperado, sem duplicidade, perda ou valor negativo.
- **SC-007**: A verificação de reconciliação cobre 100% das carteiras com movimento no período
  e reporta 100% das divergências encontradas com carteira e período identificados.
- **SC-008**: 100% das mensagens redeliveridas pela fila são reconhecidas sem duplicar o efeito
  financeiro, e 0 mensagens com falha permanente bloqueiam o processamento posterior.
- **SC-009**: Operações sobre carteiras de jogadores distintos são executadas em paralelo, sem
  que o processamento de uma carteira atrase ou impeça o de outra.
- **SC-010**: Cada operação aceita é respondida ao solicitante com o resultado e o motivo em
  código estável, e 100% das recusas são justificadas por um motivo registrado e auditável.
- **SC-011**: Uma consulta de auditoria sobre qualquer carteira reproduz, a partir do histórico,
  o saldo em qualquer data anterior, para 100% dos casos avaliados.
- **SC-012**: 100% das operações financeiras concluídas são visíveis na consulta de auditoria
  em até 5 segundos após a conclusão.

## Assumptions

<!--
  ACTION REQUIRED: The content in this section represents placeholders.
  Fill them out with the right assumptions based on reasonable defaults
  chosen when the feature description did not specify certain details.
-->

- O provedor de identidade da plataforma (OAuth 2.0 / OIDC) já está disponível e é a única
  fonte de identidade e permissão; o serviço não implementa fluxos de identidade próprios.
- Toda requisição e toda mensagem pertence a um tenant identificado, extraído do token ou do
  envelope da mensagem.
- Valores monetários são sempre compostos por inteiro na menor unidade da moeda mais o código
  ISO 4217 da moeda; a operação nunca recebe nem devolve ponto flutuante.
- O serviço é executado com múltiplas instâncias desde o início, sem modo single-node;
  dimensionamento e limitação de taxa na borda são responsabilidades da infraestrutura à frente
  do serviço.
- Falhas de infraestrutura são esperadas e recorrentes: reinício de processo, queda de rede e
  atraso na fila são cenários routineiros, não excepcionais.
- A fila (SQS) é um contrato de integração existente e conhecido; o serviço é consumidor e não
  é dono do formato das mensagens de outros sistemas.
- O histórico financeiro é preservado por toda a vida da relação com o jogador, conforme a
  política de retenção da organização.
- Sistemas externos que consomem os eventos deste serviço são tratados como idempotentes ou
  deduplicam por identificador de evento, já que a entrega é de repetição possível.
- Não há funcionalidade de saque, depósito, bônus, jackpot ou outras receitas além das cinco
  operações descritas neste documento.
- A API HTTP é REST sobre JSON com códigos de status padronizados e erros distinguíveis.
- O comportamento de retry e o tratamento de mensagens com falha permanente são responsáveis por
  consumidores bem-sucedidos e falhos permanentes permanecem disponíveis para inspeção e
  reprocessamento posterior.
- Moedas suportadas nesta versão: BRL e USD, ambas com 2 casas decimais. Uma carteira tem uma
  única moeda e nunca muda. Não há conversão de moeda nesta versão; conversão, quando existir,
  será uma operação explícita e auditada com taxa registrada.
- A ordenação das operações de uma carteira é garantida pelo agrupamento da fila por carteira;
  operações sobre carteiras distintas são independentes e processadas em paralelo.
- Número de reprocessamentos para operação cuja aposta ainda não existe: 5, com espera
  progressiva. Ao esgotar as tentativas, a mensagem é isolada para tratamento posterior e a
  recusa é registrada para auditoria.
- Saldo de carteira é sempre maior ou igual a zero, sem exceção, incluindo após ROLLBACK. Um
  ROLLBACK que exigiria saldo negativo é recusado; a correção passa por reposição de fundos.
