# Fire

Fire donne à un agent IA un environnement d'exécution isolé, sur les machines de l'entreprise.

L'agent tourne en local, chez le développeur. Le code qu'il génère s'exécute ailleurs : dans une micro-VM, sur un worker de la flotte. Rien ne sort du réseau interne.

## Pourquoi

Un agent qui écrit du code doit pouvoir l'exécuter. Aujourd'hui il y a trois options :

* Sur le poste du développeur, isolé par l'OS (Landlock, Seatbelt). Un seul environnement, pas de parallélisme, et des ressources limitées.
* Chez un fournisseur (E2B, Daytona, Modal). Simple, mais la sandbox porte les identifiants et l'accès au réseau interne. Ça, on ne le délègue pas toujours: sécurité, souveraineté, confidentialité etc..
* Sur ses propres machines. C'est Fire.

## Vocabulaire

* Sandbox — l'objet métier : un environnement demandé par un utilisateur, avec un cycle de vie et un propriétaire. Persisté en base.
* Micro-VM — l'exécution Firecracker qui matérialise une sandbox sur un worker.
* [[Worker]] — une machine de la flotte, qui fait tourner plusieurs micro-VM.

## Les responsabilités

* L'[[Orchestrateur]] choisit le worker sur lequel la micro-VM tournera, crée le job de création, l'observe, et garde la base à jour et vraie.
* Le [[Worker]] exécute les requêtes de l'[[Orchestrateur]]. C'est ici que vit la logique de création de micro-VM.
* La base de données est la source de vérité et porte la file de jobs.
* L'authentification décide à qui appartient une sandbox.
* Le réseau garde le gRPC hors de l'internet public et isole les sandboxes du réseau interne.

Les trois dernières sont détaillées dans le document [[Architecture]].

## Les contraintes

Dans l'ordre où je les arbitre quand elles s'opposent.

La fiabilité. Créer et lancer une image dans un environnement comme celui-ci a de nombreuses occasions d'échouer, que la cause soit logicielle ou matérielle. On conçoit un système comme celui-ci en partant du pire cas. Chaque garde-fou: retry, timeout, réconciliation au démarrage du worker, reaper de workers morts. Coûte de la latence.

La sécurité. Le code exécuté vient de l'utilisateur, donc il est hostile par défaut. D'où la micro-VM plutôt qu'un sandbox OS : un noyau par sandbox.

La vitesse. Cible : le cycle complet sous la seconde. Pour référence, Daytona annonce 90 ms à froid et Microsandbox <200 ms . Je n'ai pas encore mesuré les miens.

Le coût. Evidemment: optimiser les ressources déjà à notre disposition (les machines, qui sont fixes) et garder en esprit le cout des contraintes ci dessus.

## Face à E2B

E2B est la référence, et le bon choix dès qu'on a un compte cloud.

Fire est opinionné là où ils refusent de l'être. Un modèle réseau, un modèle de stockage, un modèle de routage, un playbook.

## Ce qui n'est pas fait

* Le rootfs est copié entièrement à chaque création. C'est mon premier poste de latence.
* Le scheduler ne regarde que le CPU et la mémoire, pas si le worker a déjà l'image.
* Les sandboxes ne sont pas isolées du réseau interne.
* Pas de routage vers les sandboxes sans load balancer.
* Pas de couche MCP.

## Les documents

* [[Architecture]] — mes choix techniques et pourquoi. Le point d'entrée pour comprendre le projet en profondeur.
* [[Orchestrateur]]
* [[Worker]]
