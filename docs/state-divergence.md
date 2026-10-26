# Quand la base ne reflète pas la réalité

Deux workers vivants ont été exclus pendant une heure (heartbeats refusés après une mise à jour de l'orchestrateur) sans que la base, ni personne, ne le reflète correctement. Question posée : **dans quels cas la base (`sandboxes`, `worker`) diverge-t-elle de l'état réel des machines, et le système converge-t-il seul ?**

Les mécanismes de correction (réapeurs TTL, liveness et workers, réconciliation d'inventaire au heartbeat, jetons de fencing, jobs de nettoyage, retrait des orphelins) étaient testés un par un. La suite `test/divergence` teste **la boucle complète**, sur des histoires en plusieurs étapes.

## Méthode

Un invariant, vérifié après chaque scénario :

> pour chaque sandbox : la base dit `assigned|starting|running` sur le worker W ⇔ W héberge réellement la VM (vivante).

Chaque scénario crée volontairement un écart, laisse tourner les mécanismes réels, puis vérifie que l'invariant redevient vrai dans un délai borné (20 s, ou 30 s pour B4), et que rien de faux ne s'est produit entre-temps (aucune VM saine retirée, aucun worker sain exclu).

Le vrai code des deux côtés, sauf Firecracker : l'orchestrateur (Postgres, jobs River, serveur gRPC, API HTTP, câblé par `internal/app` comme `cmd/api`) et le worker (`WorkerService`, `EventReporter`, serveur gRPC, jetons, heartbeats avec inventaire) au-dessus de `FakeVMs`, un gestionnaire de VMs en mémoire. Ni root ni KVM.

Délais d'un cluster de test : `HEARTBEAT_TIMEOUT` 2 s, liveness 2 s, worker perdu après 5 s (`SANDBOX_WORKER_LOST_TIMEOUT`), réapeurs toutes les 200 ms, heartbeats toutes les 100 ms, réessais des jobs à 50 ms. Quand un scénario vérifie un mécanisme précis, il met hors d'atteinte celui qui pourrait le doubler (liveness à 1 h), pour qu'un résultat vert ne vienne pas du mauvais chemin.

```
go test -tags integration ./test/divergence/                    # la suite (verte : aucun trou ouvert)
go test -tags integration ./test/divergence/ -run TestB4        # un scénario
go test -tags integration -race -count=3 ./test/divergence/     # stabilité
```

### La liveness se mesure sur le worker, pas sur l'horloge de l'orchestrateur

Une sandbox `assigned|starting|running` est échouée (puis nettoyée) quand **son worker ne répond plus d'elle**, dans l'un de ces deux cas (`ClaimLivenessExpired`) :

- **(a) le worker parle mais ne la liste plus** : `worker.last_heartbeat > COALESCE(last_seen_at, created_at) + SANDBOX_LIVENESS_TIMEOUT` (15 s par défaut). `last_heartbeat` et `last_seen_at` sont écrits par le `now()` de la base, dans la même transaction quand le worker liste la sandbox : un inventaire met les deux à la même valeur. Cela couvre une VM disparue sans événement (D1, E1), les états `UNKNOWN` et les heartbeats sans inventaire (B5). La mesure part de la dernière inscription du worker (`registered_at`) si elle est plus récente : un worker qui vient de redémarrer n'a pas encore eu l'occasion de lister quoi que ce soit, et les sandboxes de son processus précédent sont l'affaire du premier inventaire (« perdues »).
- **(b) le worker est parti pour de bon** : `worker.status = 'inactive'` depuis plus de `SANDBOX_WORKER_LOST_TIMEOUT` (2 min par défaut ; `inactive_since`, posé par le réapeur de workers). Cela couvre une machine perdue (A2) et un worker que l'orchestrateur refuse sans cesse (A3).

Avant, la règle était `COALESCE(last_seen_at, created_at) < now() - timeout`, sans regarder le worker : un redéploiement de l'API, une bascule Postgres ou une coupure entre l'orchestrateur et tous les workers de plus de 15 s faisait échouer **toutes** les sandboxes et retirer leurs VMs, alors que workers et VMs allaient bien (A6). Maintenant, une panne de l'orchestrateur n'enregistre ni heartbeat ni inventaire, donc ne fait rien vieillir. Après la panne, le réapeur de workers (`RunOnStart`) passe tout le monde `inactive` (`inactive_since` = l'heure du retour), puis les workers renvoient un heartbeat sous 5 s, repassent `active` et effacent `inactive_since`. Seul un worker réellement perdu reste `inactive` pendant tout le délai. `SANDBOX_WORKER_LOST_TIMEOUT` doit dépasser la plus longue panne d'orchestrateur, de base ou de réseau entre les deux qu'on accepte sans perdre de sandbox.

Un scénario qui révèle un trou **reste rouge** (pas de `t.Skip`) : il échoue avec `TROU <id> : attendu …, observé …. Voir docs/state-divergence.md#<id>`. Corriger le trou le fait passer au vert.

## Résultats

✅ le système converge comme attendu · 🔴 trou : le test échoue tant qu'il n'est pas corrigé.

### A. Worker coupé ou disparu

| # | Histoire | Attendu | Test | Résultat |
|---|---|---|---|---|
| A1 | 2 sandboxes, crash du worker, retour 1 s plus tard | rien n'est nettoyé, elles restent `running` | `TestA1_WorkerCrashesAndComesBack` | ✅ |
| A2 | la machine est perdue (VMs comprises) | worker `inactive`, puis, `SANDBOX_WORKER_LOST_TIMEOUT` plus tard, sandboxes `failed` et `worker_id` vidé ; les nouvelles vont sur l'autre worker | `TestA2_MachineLost` | ✅ (voir constats 1 et 7) |
| A3 | le worker est remplacé par un vieux binaire refusé (heartbeats sans jeton) dont le gRPC répond encore, puis un vrai worker revient | sandbox `failed` une fois le worker exclu depuis `SANDBOX_WORKER_LOST_TIMEOUT`, VM retirée, invariant vrai à la fin | `TestA3_WorkerReplacedByARefusedOldBinary` | ✅ (voir constat 7) |
| A4 | TTL court, coupure réseau dans les deux sens pendant l'expiration, puis retour | sandbox terminale sans worker, VM retirée comme orpheline au retour | `TestA4_TTLExpiresDuringAPartition` | ✅ mais termine `failed`, pas `stopped` (voir constat 2) |
| A5 | coupure de 1 s (moins que tous les délais) | rien ne bouge | `TestA5_ShortPartitionChangesNothing` | ✅ |
| A6 | l'orchestrateur est injoignable pour **tous** les workers pendant 3 s (plus que la liveness et `HEARTBEAT_TIMEOUT`, moins que le délai de worker perdu) : redéploiement, crash, bascule Postgres | rien n'est échoué ni retiré : les sandboxes restent `running`, les VMs aussi | `TestA6_OrchestratorOutageKillsNothing` | ✅ depuis le correctif (constat 7 ; mesuré avant : les deux sandboxes `failed`, les deux VMs retirées) |

### B. Redémarrages et jetons

| # | Histoire | Attendu | Test | Résultat |
|---|---|---|---|---|
| B1 | crash, des VMs meurent pendant l'arrêt (l'une listée `DEAD` au retour, l'autre plus listée du tout), redémarrage | les deux `failed` dès le premier inventaire, sans attendre la liveness | `TestB1_VMsDieWhileTheWorkerIsDown` | ✅ (≈ 30 ms après le redémarrage) |
| B2 | le worker héberge une VM inconnue de la base | `RemoveSandbox` reçu, VM retirée | `TestB2_UnknownVMIsRemoved` | ✅ |
| B3 | deux processus avec le même ID, l'ancien continue | l'ancien ne modifie ni la ligne worker, ni les sandboxes, ni ne stocke d'événement, et on n'agit pas sur son inventaire | `TestB3_StaleProcessCannotModifyAnything` | ✅ |
| B4 | worker sain, puis son `fencing_token` est remis à zéro en base (base restaurée) | le worker revient (nouvel enregistrement) sous 30 s | `TestB4_WorkerRecoversFromALostFencingToken` | ✅ depuis le correctif ([voir B4](#b4)) |
| B5 | heartbeats sans `inventory_complete` | aucune réconciliation (une VM inconnue reste), seule la liveness s'applique | `TestB5_HeartbeatsWithoutAnInventory` | ✅ |

### C. Créations et suppressions interrompues

| # | Histoire | Attendu | Test | Résultat |
|---|---|---|---|---|
| C1 | la réponse de `CreateSandbox` est perdue (la VM existe) | le job réessaie, **une seule** VM bootée, `running` | `TestC1_LostCreateResponseBootsOneVM` | ✅ |
| C2 | la VM est créée mais toutes les tentatives du job échouent (réponses perdues) | `cleanup_pending`, `RemoveSandbox`, plus de VM, `failed` | `TestC2_CreateJobExhaustsItsAttemptsAfterBooting` | ✅ (voir constat 5) |
| C3 | le worker refuse (`ResourceExhausted`) | `failed`, rien sur le worker | `TestC3_WorkerRefusesTheCreate` | ✅ (voir constat 5) |
| C5 | la liveness échoue la sandbox entre deux tentatives du job de création (worker lent qui refuse) | le job est annulé, l'API répond 502, pas 201 | `TestC5_SandboxFailedUnderItsCreateJobIsNotReportedCreated` | ✅ depuis le correctif (constat 5) |
| C4 | suppression pendant que le worker est injoignable, puis retour | abandon (`worker_id` vidé), VM retirée comme orpheline au retour | `TestC4_DeleteWhileTheWorkerIsUnreachable` | ✅ mais termine `failed`, pas `stopped` (voir constat 2) |

### D. Dérive d'une sandbox

| # | Histoire | Attendu | Test | Résultat |
|---|---|---|---|---|
| D1 | une VM meurt et son événement est perdu | l'inventaire seul la passe `failed` et la nettoie | `TestD1_LostDeathEventIsCaughtByTheInventory` | ✅ |
| D2 | événements dupliqués, dans le désordre, sur une sandbox supprimée ou inconnue, avec un mauvais jeton | les gardes tiennent, rien ne ressuscite, l'inventaire converge | `TestD2_DuplicatedAndMisorderedEvents` | ✅ (voir constat 4) |
| D3 | sandbox bloquée en `pending`/`scheduling`, sans job | échouée par le délai de création, bien avant son TTL | `TestD3_StalledCreationIsFailedByTheCreateTimeout` | ✅ depuis le correctif (constat 3 ; mesuré avant : elle attendait son TTL) |

### E. Capacité faussée

| # | Histoire | Attendu | Test | Résultat |
|---|---|---|---|---|
| E1 | capacité 1, la VM disparaît sans événement, puis nouvelle création | la place se libère après réconciliation, la création passe | `TestE1_VanishedVMFreesItsSlotAfterReconciliation` | ✅ (≈ 2,5 s) |
| E2 | la capacité baisse sous le nombre hébergé | refus des nouvelles créations, l'existant n'est pas touché | `TestE2_CapacityBelowWhatIsHosted` | ✅ |

## Trous

Aucun trou ouvert : toute la suite est verte. Un trou qui apparaîtrait fait échouer son scénario par `gap(...)` et vient se documenter ici.

### B4

**Corrigé** (scénario `TestB4_WorkerRecoversFromALostFencingToken`, ≈ 0,2 s pour que le worker soit de nouveau enregistré et son inventaire réconcilié ; 30 s était le délai accordé).

**Attendu** : un worker sain dont la base a perdu le jeton (ligne supprimée, ou base restaurée depuis une sauvegarde plus ancienne) se réenregistre seul et redevient `active` sous 30 s.

**Observé avant le correctif** : il ne revenait jamais. Après `UPDATE worker SET fencing_token = 0, inventory_token = 0`, le worker (qui garde son jeton 1 en mémoire) envoyait ses heartbeats, `UpdateWorker` ne trouvait aucune ligne avec `fencing_token = 1` et répondait `FailedPrecondition` à vie (293 heartbeats refusés en 30 s). Le worker ne s'enregistrait que si **son** jeton valait 0, ce qui n'arrivait qu'au démarrage du processus. Pire : l'inventaire étant refusé, la liveness échouait ensuite les sandboxes de ce worker alors que leurs VMs tournaient, et on retirait des VMs saines.

**Correctif** : le refus `FailedPrecondition`, qui confondait deux cas, est séparé.

- `worker.Repository.Update`, quand aucune ligne ne correspond au jeton présenté, relit le jeton de la ligne avec le même handle (la transaction du heartbeat). Ligne absente, ou jeton en base **inférieur** au jeton présenté : `ErrWorkerUnknown`, que le serveur gRPC répond `NotFound` (« worker unknown, register again »). Jeton en base **supérieur** : `ErrFenced` → `FailedPrecondition`, refus définitif comme avant.
- Le worker (`needsRegister`, vrai au démarrage) se réenregistre au heartbeat suivant quand il reçoit `NotFound`, en présentant le dernier jeton qu'il détenait. `Register` pose `GREATEST(jeton en base, jeton présenté) + 1` : le nouveau jeton dépasse tout ce qu'un processus a jamais détenu, donc un ancien processus (jeton inférieur) reste refusé.
- B3 reste vert : c'est lui qui garantit qu'un vieux processus ne peut pas se réenregistrer à la place du courant.

## Constats en cours de route

Ce ne sont pas des échecs du catalogue (tous les tests concernés sont verts), mais des comportements que les scénarios ont fait apparaître. Ceux qui sont marqués « Corrigé » l'ont été ; les autres (1, 2, 4) restent à traiter.

1. **Le scheduler écarte un worker après 15 s codés en dur** (`buildCandidates` appelle `HeartbeatExpired(…, 0)`, donc le défaut de 15 s), quel que soit `HEARTBEAT_TIMEOUT`. Dans A2 on ne le voit pas : une machine perdue refuse la connexion à `GetWorkerInfo`, elle n'est donc pas candidate du tout, et les nouvelles sandboxes vont sur l'autre worker dès le premier essai. La fenêtre de 15 s ne compte que pour un worker qui répond encore au gRPC sans plus envoyer de heartbeats acceptés (A3). *Lecture du code, non éprouvé par un scénario.* À noter dans la même fonction : quand aucun worker `active` n'est joignable, `ChooseLeastUsedWorker` retombe sur n'importe quel candidat, **y compris un worker `inactive`** qui répond.
   Délais mesurés pour A2 sous la charge de la suite (21 scénarios en parallèle) : worker `inactive` ≈ 2,1 s après la perte de la machine (timeout 2 s + réapeur 200 ms), sandboxes `failed` ≈ 11 s. Ce second délai a changé avec le constat 7 : les sandboxes d'un worker `inactive` ne sont plus échouées par la liveness (2 s) mais une fois le worker perdu, soit 5 s dans la suite après qu'il a été vu `inactive` (≈ 7,1 s), puis le nettoyage, dont les 5 tentatives vers une machine qui refuse la connexion durent ≈ 4 s (River relance une tentative environ chaque seconde, quel que soit le backoff de 50 ms de la suite). Mesuré avant : ≈ 7,9 s, dont 4 s de nettoyage. En production : 15 s de heartbeat + `SANDBOX_WORKER_LOST_TIMEOUT` (2 min) + le nettoyage.
2. **A4 et C4 finissent `failed`, pas `stopped`**, alors que le TTL ou l'appel `DELETE` demandait `stopped`. C'est le comportement documenté de `CleanupSandboxWorker` : quand le retrait n'est pas confirmé à la dernière tentative, la sandbox finit `failed` quelle que soit la cible. Dans la suite, les réessais à 50 ms épuisent les 5 tentatives en une fraction de seconde ; en production le backoff exponentiel (2 s, 4 s, 8 s, 16 s, plafonné à 30 s) laisserait au worker le temps de revenir et de confirmer, et la sandbox finirait `stopped`. Les tests vérifient donc ce qui tient dans les deux cas : état terminal, `worker_id` vidé, VM retirée au retour.
3. **D3 : une sandbox sans job, bloquée en `scheduling` (ou `pending`), attendait la fin de son TTL. Corrigé** (branche `fix/stalled-creation-reaper`). La liveness ne regarde que les sandboxes `assigned|starting|running` qui ont un `worker_id`. Mesuré avant le correctif avec un TTL de 4 s : fin de vie à ≈ 4 s, pas à 2 s (liveness) ; avec le TTL par défaut, une sandbox dont le job a été abandonné (par exemple parce que `maybeCleanup` n'a pas pu joindre la base à la dernière tentative) restait « en cours de création » pendant une heure. Le réapeur de sandboxes réclame maintenant aussi les sandboxes `pending|scheduling` plus vieilles que `SANDBOX_CREATE_TIMEOUT` (5 min par défaut, `0` désactive le contrôle) et les mène à `failed` par le job de nettoyage habituel. Le délai doit dépasser le temps maximal qu'une sandbox saine passe en `pending`/`scheduling` : l'étape d'ordonnancement de son job (5 tentatives, 5 s de timeout gRPC pour tous les candidats ensemble, backoff 2 + 4 + 8 + 16 s, soit environ une minute) plus l'attente dans la file River quand elle est chargée. L'appel `CreateSandbox` (45 s par tentative, voir plus bas) vient après `assigned` et ne compte pas : à partir de là, c'est la liveness qui veille.
4. **D2 : un événement renvoyé avec le même ID est refusé en `Internal`** (violation de clé primaire sur `sandbox_events.id`), pas traité comme un doublon. Sans conséquence aujourd'hui, car le worker ne renvoie jamais un événement. À savoir le jour où il le fera.
5. **La liveness pouvait échouer une sandbox sous les pieds du job de création, et l'API répondait alors `201` pour une sandbox `failed`. Corrigé** (branche `fix/create-job-terminal-status`, scénario C5). Une sandbox `assigned` que le worker ne liste pas vieillit ; le réapeur la passait en `cleanup_pending` entre deux tentatives du job ; la tentative suivante voyait un statut terminal et rendait `nil` (« déjà terminal, no-op »), River marquait le job terminé et le handler HTTP renvoyait `201 Created` avec une sandbox `failed`. En production : 15 s de liveness contre un backoff de 2 s à 30 s entre les tentatives, avec un réapeur qui passe toutes les minutes. Le job rend désormais `river.JobCancel` pour toute sandbox qui a fini sans démarrer (`failed`, `stopped`, `cleanup_pending`, `cleaned_up`), ce qui donne le `502` existant et empêche River de réessayer ; seul `running` reste un succès. Les trois endroits où une mise à jour gardée (`UpdateStatus`, `AssignWorker`) ne change rien, parce que la sandbox a bougé entre la lecture et l'écriture (un `DELETE`, le TTL), renvoyaient eux aussi `nil` : ils relisent maintenant la sandbox et disent ce qu'elle est devenue (`movedUnder`). C2 et C3 gardent la liveness hors d'atteinte pour tester leur propre mécanisme, sans dépendre de la charge de la machine.
6. **Le scheduler comparait des unités différentes. Corrigé** (trouvé en lisant `hasCapacity`, hors du périmètre de la suite). `cpu_usage` est la charge en pourcentage de la machine (`readCPUUsagePercent`, 0 à 100) et `cpu_budget` vaut par défaut le nombre de cœurs (`runtime.NumCPU()`, `WORKER_CPU_BUDGET` n'est pas posé par Ansible). `hasCapacity` calculait `usage / budget` : un worker à 2 cœurs était jugé plein dès 2 % de charge, un worker à 4 cœurs dès 4 % (vérifié par un test jetable : 2 cœurs à 1 % → place, à 2 % → plein ; 4 cœurs à 3 % → place, à 4 % → plein). Résultat : `ErrClusterFull` et un `503 no worker capacity available` pour un worker presque au repos. Le ratio CPU vaut maintenant `cpu_usage / 100`, dans `hasCapacity`, `WorkerLoadScore` et `WorkerWeight` (`cpuUsageScale`) ; la mémoire ne change pas, `mem_usage` et `mem_budget` étant tous deux en MiB. Tests : 2 cœurs à 50 % → place, 4 cœurs à 99 % → place, 100 % → plein, et les poids suivent la charge en pourcentage quel que soit le nombre de cœurs. La suite continue de neutraliser la charge de l'hôte (`GetWorkerInfo` du `SimWorker` renvoie 0) pour que la charge réelle de la machine de test n'influe pas sur les scénarios. *Non éprouvé sur un vrai worker.*
7. **La liveness se mesurait contre l'horloge de l'orchestrateur : une panne de l'orchestrateur tuait toutes les sandboxes saines. Corrigé** (scénario A6, voir « La liveness se mesure sur le worker » plus haut). `ClaimLivenessExpired` échouait toute sandbox `assigned|starting|running` dont `COALESCE(last_seen_at, created_at)` avait plus de 15 s, sans regarder si c'était l'orchestrateur qui n'avait pas pu recevoir les heartbeats. Un redéploiement de l'API, un crash, une bascule Postgres ou une coupure de plus de 15 s entre l'orchestrateur et tous les workers faisait échouer toutes les sandboxes et retirer leurs VMs, workers et VMs allant bien ; le réapeur, lancé aussi au démarrage (`RunOnStart`), passait avant que les workers aient pu renvoyer un heartbeat. Mesuré avant le correctif avec A6 : les deux sandboxes `failed`, les deux VMs retirées. Le correctif joint la table `worker` : une sandbox n'est réclamée que si son worker parle sans la lister (`last_heartbeat` plus récent que `last_seen_at` de plus de `SANDBOX_LIVENESS_TIMEOUT`) ou s'il est `inactive` depuis plus de `SANDBOX_WORKER_LOST_TIMEOUT` (migration 014 : `worker.inactive_since`). `last_heartbeat` est maintenant écrit par le `now()` de la base (`Update` et `Register`) comme `last_seen_at`, pour que la règle compare une horloge à elle-même ; il dépend donc du `now()` de la base, y compris pour le scheduler (`HeartbeatExpired`, 15 s en dur), qui le compare à l'horloge de l'API : un écart d'horloge de plusieurs secondes entre l'hôte de l'API et celui de la base le fausserait. A2 et A3 passent désormais par le délai de worker perdu (leurs durées ont changé : voir constat 1) ; A2 vérifie en plus que les sandboxes sont encore `running` quand le worker vient d'être vu `inactive`.
8. **Les appels de l'orchestrateur vers un worker avaient un délai propre seulement pour le choix des candidats (5 s). Corrigé** : `CreateSandbox` est borné à 45 s (le boot est borné à 30 s côté worker), le retrait (`removeSandboxOnWorker`, utilisé aussi pour les orphelins) à 15 s (arrêt : 3 s de grâce puis kill, réessais `EBUSY` de dm jusqu'à 5 s). Un worker injoignable par perte de paquets (et non par refus de connexion) bloquait le job jusqu'au `JobTimeout` de River. Un délai dépassé est une erreur : le job de création reprend à `assigned` (le `Create` du worker est idempotent), le nettoyage reste `cleanup_pending` tant que le retrait n'est pas confirmé. Le `JobTimeout` du client River est posé explicitement à 80 s (`orchestrator.JobTimeout` : 5 s + 45 s + 30 s de marge pour la base) ; le défaut de River (1 min) ne laissait que 10 s.

## Contre-épreuve

Pour s'assurer qu'un scénario vert est vert pour la bonne raison, on neutralise le mécanisme qu'il teste (`go test -overlay`, sans modifier le code) et il doit passer au rouge :

| Mécanisme neutralisé ou remplacé (`sandbox/repository_reconcile.go`) | Scénario | Résultat |
|---|---|---|
| requête des orphelins de `ReconcileInventory` | B2 | 🔴 « the unknown VM is still on the worker after 20s » |
| traitement des VMs `DEAD` de l'inventaire | D1 | 🔴 « status running after 20s, want [failed] » |
| `Lost` du premier inventaire | B1 | 🔴 la sandbox « non listée » reste `running` ; celle « listée DEAD » passe, ce qui montre que les deux chemins sont bien distincts |
| règle de liveness d'avant le constat 7 (`COALESCE(last_seen_at, created_at) < now() - timeout`, sans le worker) | A6 | 🔴 « healthy sandbox … ended failed » ×2, « healthy VMs were removed » ; A2 passe au rouge aussi (« sandbox … is cleanup_pending as soon as w1 is shown inactive »), A3 reste vert (≈ 5,5 s au lieu de ≈ 10 s) |

## Durée

≈ 32 s pour la suite complète (21 scénarios en parallèle, un Postgres par scénario). Le plus long, A2 (≈ 15 s), attend le délai de worker perdu puis les tentatives du nettoyage. B4 n'attend plus ses 30 s : il passe en ≈ 2,5 s. Sous `-race -count=3`, ≈ 1 min 54 pour les trois passages (114 s), sans data race.
