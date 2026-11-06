//! Windows-only parts of the agent: service, helper process, installer and secret storage.
pub mod acl;
pub mod dpapi;
pub mod install;
pub mod service;
pub mod spawn;
pub mod sys;
pub mod update;
