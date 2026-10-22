# Quand la base ne reflète pas la réalité

Deux workers vivants ont été exclus pendant une heure (heartbeats refusés après une mise à jour de l'orchestrateur) sans que la base, ni personne, ne le reflète correctement. Question posée : **dans quels cas la base (`sandboxes`, `worker`) diverge-t-elle de l'état réel des machines, et le système converge-t-il seul ?**

Les mécanismes de correction (réapeurs TTL, liveness et workers, réconciliation d'inventaire au heartbeat, jetons de fencing, jobs de nettoyage, retrait des orphelins) étaient testés un par un. La suite `test/divergence` teste **la boucle complète**, sur des histoires en plusieurs étapes.

## Méthode

Un invariant, vérifié après chaque scénario :

> pour chaque sandbox : la base dit `assigned|starting|running` sur le worker W ⇔ W héberge réellement la VM (vivante).

Chaque scénario crée volontairement un écart, laisse tourner les mécanismes réels, puis vérifie que l'invariant redevient vrai dans un délai borné (20 s, ou 30 s pour B4), et que rien de faux ne s'est produit entre-temps (aucune VM saine retirée, aucun worker sain exclu).

Le vrai code des deux côtés, sauf Firecracker : l'orchestrateur (Postgres, jobs River, serveur gRPC, API HTTP, câblé par `internal/app` comme `cmd/api`) et le worker (`WorkerService`, `EventReporter`, serveur gRPC, jetons, heartbeats avec inventaire) au-dessus de `FakeVMs`, un gestionnaire de VMs en mémoire. Ni root ni KVM.

Délais d'un cluster de test : `HEARTBEAT_TIMEOUT` 2 s, liveness 2 s, réapeurs toutes les 200 ms, heartbeats toutes les 100 ms, réessais des jobs à 50 ms. Quand un scénario vérifie un mécanisme précis, il met hors d'atteinte celui qui pourrait le doubler (liveness à 1 h), pour qu'un résultat vert ne vienne pas du mauvais chemin.

```
go test -tags integration ./test/divergence/                    # la suite (rouge tant que B4 existe)
go test -tags integration ./test/divergence/ -run TestB4        # un scénario
go test -tags integration -race -count=3 ./test/divergence/     # stabilité
```

Un scénario qui révèle un trou **reste rouge** (pas de `t.Skip`) : il échoue avec `TROU <id> : attendu …, observé …. Voir docs/state-divergence.md#<id>`. Corriger le trou le fait passer au vert.

## Résultats

✅ le système converge comme attendu · 🔴 trou : le test échoue tant qu'il n'est pas corrigé.

### A. Worker coupé ou disparu

| # | Histoire | Attendu | Test | Résultat |
|---|---|---|---|---|
| A1 | 2 sandboxes, crash du worker, retour 1 s plus tard | rien n'est nettoyé, elles restent `running` | `TestA1_WorkerCrashesAndComesBack` | ✅ |
| A2 | la machine est perdue (VMs comprises) | worker `inactive`, sandboxes `failed` et `worker_id` vidé, les nouvelles vont sur l'autre worker | `TestA2_MachineLost` | ✅ (voir constat 1) |
| A3 | le worker est remplacé par un vieux binaire refusé (heartbeats sans jeton) dont le gRPC répond encore, puis un vrai worker revient | sandbox `failed` pendant l'exclusion, VM retirée, invariant vrai à la fin | `TestA3_WorkerReplacedByARefusedOldBinary` | ✅ |
| A4 | TTL court, coupure réseau dans les deux sens pendant l'expiration, puis retour | sandbox terminale sans worker, VM retirée comme orpheline au retour | `TestA4_TTLExpiresDuringAPartition` | ✅ mais termine `failed`, pas `stopped` (voir constat 2) |
| A5 | coupure de 1 s (moins que tous les délais) | rien ne bouge | `TestA5_ShortPartitionChangesNothing` | ✅ |

### B. Redémarrages et jetons

| # | Histoire | Attendu | Test | Résultat |
|---|---|---|---|---|
| B1 | crash, des VMs meurent pendant l'arrêt (l'une listée `DEAD` au retour, l'autre plus listée du tout), redémarrage | les deux `failed` dès le premier inventaire, sans attendre la liveness | `TestB1_VMsDieWhileTheWorkerIsDown` | ✅ (≈ 30 ms après le redémarrage) |
| B2 | le worker héberge une VM inconnue de la base | `RemoveSandbox` reçu, VM retirée | `TestB2_UnknownVMIsRemoved` | ✅ |
| B3 | deux processus avec le même ID, l'ancien continue | l'ancien ne modifie ni la ligne worker, ni les sandboxes, ni ne stocke d'événement, et on n'agit pas sur son inventaire | `TestB3_StaleProcessCannotModifyAnything` | ✅ |
| B4 | worker sain, puis son `fencing_token` est remis à zéro en base (base restaurée) | le worker revient (nouvel enregistrement) sous 30 s | `TestB4_WorkerRecoversFromALostFencingToken` | 🔴 [voir B4](#b4) |
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

### B4

**Attendu** : un worker sain dont la base a perdu le jeton (ligne supprimée, ou base restaurée depuis une sauvegarde plus ancienne) se réenregistre seul et redevient `active` sous 30 s.

**Observé** : il ne revient jamais. Après `UPDATE worker SET fencing_token = 0, inventory_token = 0`, le worker (qui garde son jeton 1 en mémoire) envoie ses heartbeats, `UpdateWorker` ne trouve aucune ligne avec `fencing_token = 1` et répond `FailedPrecondition` à vie. Le worker ne s'enregistre que si **son** jeton vaut 0 (`worker/monitoring.go`, `UpdateWorker`), ce qui n'arrive qu'au démarrage du processus. Pire : l'inventaire étant refusé, la liveness échoue ensuite les sandboxes de ce worker alors que leurs VMs tournent (le test l'observe : `sandbox … failed`), et on retire des VMs saines.

Le test échoue de façon stable (même message aux 3 passages de `-race -count=3`).

**Piste de correction** : distinguer deux refus aujourd'hui confondus dans `FailedPrecondition`.

- jeton présenté **inférieur** à celui de la base, ou égal : un ancien processus (B3) → refus définitif, comme aujourd'hui ;
- jeton présenté **supérieur** à celui de la base, ou ligne absente : c'est la base qui est en retard → réponse distincte (par exemple `NotFound` avec le motif « worker unknown »), et le worker remet son jeton à 0 pour se réenregistrer au heartbeat suivant (`RegisterWorker` incrémente à partir de la valeur en base).

Une branche par trou, base `chore/hardening`. B3 doit rester vert : c'est lui qui garantit qu'un vieux processus ne peut pas se réenregistrer à la place du courant.

## Constats en cours de route

Ce ne sont pas des échecs du catalogue (tous les tests concernés sont verts), mais des comportements que les scénarios ont fait apparaître. Rien n'est corrigé dans cette branche.

1. **Le scheduler écarte un worker après 15 s codés en dur** (`buildCandidates` appelle `HeartbeatExpired(…, 0)`, donc le défaut de 15 s), quel que soit `HEARTBEAT_TIMEOUT`. Dans A2 on ne le voit pas : une machine perdue refuse la connexion à `GetWorkerInfo`, elle n'est donc pas candidate du tout, et les nouvelles sandboxes vont sur l'autre worker dès le premier essai. La fenêtre de 15 s ne compte que pour un worker qui répond encore au gRPC sans plus envoyer de heartbeats acceptés (A3). *Lecture du code, non éprouvé par un scénario.* À noter dans la même fonction : quand aucun worker `active` n'est joignable, `ChooseLeastUsedWorker` retombe sur n'importe quel candidat, **y compris un worker `inactive`** qui répond.
   Délais mesurés pour A2 sous la charge de la suite (19 scénarios en parallèle) : worker `inactive` ≈ 3,8 s après la perte de la machine (timeout 2 s + réapeur 200 ms), sandboxes `failed` ≈ 7,9 s.
2. **A4 et C4 finissent `failed`, pas `stopped`**, alors que le TTL ou l'appel `DELETE` demandait `stopped`. C'est le comportement documenté de `CleanupSandboxWorker` : quand le retrait n'est pas confirmé à la dernière tentative, la sandbox finit `failed` quelle que soit la cible. Dans la suite, les réessais à 50 ms épuisent les 5 tentatives en une fraction de seconde ; en production le backoff exponentiel (2 s, 4 s, 8 s, 16 s, plafonné à 30 s) laisserait au worker le temps de revenir et de confirmer, et la sandbox finirait `stopped`. Les tests vérifient donc ce qui tient dans les deux cas : état terminal, `worker_id` vidé, VM retirée au retour.
3. **D3 : une sandbox sans job, bloquée en `scheduling` (ou `pending`), attendait la fin de son TTL. Corrigé** (branche `fix/stalled-creation-reaper`). La liveness ne regarde que les sandboxes `assigned|starting|running` qui ont un `worker_id`. Mesuré avant le correctif avec un TTL de 4 s : fin de vie à ≈ 4 s, pas à 2 s (liveness) ; avec le TTL par défaut, une sandbox dont le job a été abandonné (par exemple parce que `maybeCleanup` n'a pas pu joindre la base à la dernière tentative) restait « en cours de création » pendant une heure. Le réapeur de sandboxes réclame maintenant aussi les sandboxes `pending|scheduling` plus vieilles que `SANDBOX_CREATE_TIMEOUT` (5 min par défaut, `0` désactive le contrôle) et les mène à `failed` par le job de nettoyage habituel. Le délai doit dépasser la durée maximale d'un job sain : 5 tentatives, backoff jusqu'à 30 s, 5 s de timeout gRPC par candidat.
4. **D2 : un événement renvoyé avec le même ID est refusé en `Internal`** (violation de clé primaire sur `sandbox_events.id`), pas traité comme un doublon. Sans conséquence aujourd'hui, car le worker ne renvoie jamais un événement. À savoir le jour où il le fera.
5. **La liveness pouvait échouer une sandbox sous les pieds du job de création, et l'API répondait alors `201` pour une sandbox `failed`. Corrigé** (branche `fix/create-job-terminal-status`, scénario C5). Une sandbox `assigned` que le worker ne liste pas vieillit ; le réapeur la passait en `cleanup_pending` entre deux tentatives du job ; la tentative suivante voyait un statut terminal et rendait `nil` (« déjà terminal, no-op »), River marquait le job terminé et le handler HTTP renvoyait `201 Created` avec une sandbox `failed`. En production : 15 s de liveness contre un backoff de 2 s à 30 s entre les tentatives, avec un réapeur qui passe toutes les minutes. Le job rend désormais `river.JobCancel` pour toute sandbox qui a fini sans démarrer (`failed`, `stopped`, `cleanup_pending`, `cleaned_up`), ce qui donne le `502` existant et empêche River de réessayer ; seul `running` reste un succès. Les trois endroits où une mise à jour gardée (`UpdateStatus`, `AssignWorker`) ne change rien, parce que la sandbox a bougé entre la lecture et l'écriture (un `DELETE`, le TTL), renvoyaient eux aussi `nil` : ils relisent maintenant la sandbox et disent ce qu'elle est devenue (`movedUnder`). C2 et C3 gardent la liveness hors d'atteinte pour tester leur propre mécanisme, sans dépendre de la charge de la machine.
6. **Hors du périmètre de la suite, trouvé en lisant `hasCapacity` : le scheduler compare des unités différentes.** `cpu_usage` est la charge en pourcentage de la machine (`readCPUUsagePercent`, 0 à 100) et `cpu_budget` vaut par défaut le nombre de cœurs (`runtime.NumCPU()`, `WORKER_CPU_BUDGET` n'est pas posé par Ansible). `hasCapacity` calcule `usage / budget` : un worker à 2 cœurs est jugé plein dès 2 % de charge, un worker à 4 cœurs dès 4 % (vérifié par un test jetable : 2 cœurs à 1 % → place, à 2 % → plein ; 4 cœurs à 3 % → place, à 4 % → plein). Résultat : `ErrClusterFull` et un `503 no worker capacity available` pour un worker presque au repos. La suite neutralise la charge de l'hôte (`GetWorkerInfo` du `SimWorker` renvoie 0) pour que ce comportement n'influe pas sur les scénarios. *À confirmer sur un vrai worker avant de corriger.*

## Contre-épreuve

Pour s'assurer qu'un scénario vert est vert pour la bonne raison, on neutralise le mécanisme qu'il teste (`go test -overlay`, sans modifier le code) et il doit passer au rouge :

| Mécanisme neutralisé (`sandbox/repository_reconcile.go`) | Scénario | Résultat |
|---|---|---|
| requête des orphelins de `ReconcileInventory` | B2 | 🔴 « the unknown VM is still on the worker after 20s » |
| traitement des VMs `DEAD` de l'inventaire | D1 | 🔴 « status running after 20s, want [failed] » |
| `Lost` du premier inventaire | B1 | 🔴 la sandbox « non listée » reste `running` ; celle « listée DEAD » passe, ce qui montre que les deux chemins sont bien distincts |

## Durée

≈ 37 s pour la suite complète (19 scénarios en parallèle, un Postgres par scénario) ; B4 attend ses 30 s. Sous `-race -count=3`, ≈ 2 min pour les trois passages (122 s), sans data race.
