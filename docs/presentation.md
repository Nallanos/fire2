# Fire

Fire donne à un agent IA un environnement d'exécution isolé, sur les machines de l'entreprise.

L'agent tourne en local, chez le développeur. Le code qu'il génère s'exécute ailleurs : dans une micro-VM, sur un worker de la flotte. Rien ne sort du réseau interne.

## Pourquoi

Un agent qui écrit du code doit pouvoir l'exécuter. Aujourd'hui il y a trois options :

* sur le poste du développeur, isolé par l'OS (Landlock, Seatbelt). Un seul environnement, pas de parallélisme, et le laptop n'est pas la bonne machine.
* chez un fournisseur (E2B, Daytona, Modal). Simple, mais la sandbox porte les identifiants et l'accès au réseau interne. Ça, on ne le délègue pas toujours.
* sur ses propres machines. C'est Fire.

## Vocabulaire

* **sandbox** — l'objet métier : un environnement demandé par un utilisateur, avec un cycle de vie et un propriétaire. Persisté en base.
* **micro-VM** — l'exécution Firecracker qui matérialise une sandbox sur un worker.
* **worker** — une machine de la flotte, qui fait tourner plusieurs micro-VM.

## Les responsabilités

* **L'orchestrateur** choisit le worker sur lequel la micro-VM tournera, crée le job de création, l'observe, et garde la base à jour et vraie.
* **Le worker** exécute les requêtes de l'orchestrateur. C'est ici que vit la logique de création de micro-VM.
* **La base** est la source de vérité et porte la file de jobs.
* **L'authentification** décide à qui appartient une sandbox.
* **Le réseau** garde le gRPC hors de l'internet public et isole les sandboxes du réseau interne.

Les trois dernières sont détaillées dans le document d'architecture.

## Les contraintes

Dans l'ordre où je les arbitre quand elles s'opposent.

**La fiabilité.** Créer et lancer une image dans un environnement comme celui-ci a de nombreuses occasions d'échouer, que la cause soit logicielle ou matérielle. On conçoit un système comme celui-ci en partant du pire cas. Chaque garde-fou — retry, timeout, réconciliation au démarrage du worker, reaper de workers morts — coûte de la latence, et je paie ce prix.

**La sécurité.** Le code exécuté vient de l'utilisateur, donc il est hostile par défaut. D'où la micro-VM plutôt qu'un sandbox OS : un noyau par sandbox, et des tenants qui ne se font pas confiance entre eux. Un sandbox OS a des évasions ; sur un poste mono-utilisateur c'est acceptable, sur une flotte partagée non.

**La vitesse.** Cible : le cycle complet sous la seconde. Pour référence, Daytona annonce ~90 ms à froid (gVisor) et Microsandbox <200 ms (libkrun). Je n'ai pas encore mesuré les miens.

**Le coût.** La flotte est fixe, il n'y a pas d'autoscaling. C'est ce qui rend le placement important : dans le cloud, un démarrage lent se compense en ajoutant une machine ; sur seize machines dans un rack, non.

## Non-objectifs

* Les images utilisateur arbitraires. L'utilisateur apporte du code, pas une image — incompatible avec la cible de démarrage.
* Le multi-région, l'autoscaling, la persistance disque entre deux runs.
* Kubernetes.

## Face à E2B

E2B est la référence, et le bon choix dès qu'on a un compte cloud. Leur code tourne sur une machine Linux nue — le mainteneur le dit — mais il n'existe aucun outillage pour ça, et c'est assumé : ils ne veulent pas imposer d'opinions sur la sécurité réseau et les backends de stockage de leurs clients.

Fire est opinionné là où ils refusent de l'être. Un modèle réseau, un modèle de stockage, un modèle de routage, un playbook.

Ils citent aussi deux problèmes qu'ils n'ont pas résolus, faute de tourner on-prem eux-mêmes : la sécurité réseau des sandboxes et le load balancing. Ce sont mes chantiers.

## Ce qui n'est pas fait

* Le rootfs est copié entièrement à chaque création. C'est mon premier poste de latence.
* Le scheduler ne regarde que le CPU et la mémoire, pas si le worker a déjà l'image.
* Les sandboxes ne sont pas isolées du réseau interne.
* Pas de routage vers les sandboxes sans load balancer.
* Pas de couche MCP.

## Les documents

* **Architecture** — mes choix techniques et pourquoi. Le point d'entrée pour comprendre le projet en profondeur.
* **Orchestrateur**
* **Worker**
