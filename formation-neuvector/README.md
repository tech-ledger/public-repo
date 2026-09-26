# NeuVector en production : le dépôt GitOps

Les manifestes du module 03 de la formation
[NeuVector en production](https://techledger.io/formations/scuriser-les-environnements-kubernetes-avec-suse-security-neuvector-du-dploiement-standalone-et-fdr-la-mise-en-pratique),
que suit Argo CD :

- `apps/` : les ApplicationSets, qui ciblent les clusters dont le Secret de
  cluster Argo CD porte le label `neuvector.techledger.io/role: managed` ;
- `charts/federation/` : le Secret de fédération, lu dans Vault par External
  Secrets, posé avant NeuVector ;
- `values/` : les values communes du chart NeuVector.

Aucun secret n'est dans ce dépôt : le jeton de jonction et les mots de passe
sont dans Vault.
