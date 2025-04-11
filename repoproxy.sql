create database repoproxy;

create table public.cacheitem
(
    rowid        bigserial
        constraint cacheitem_pk
            primary key,
    reponame     varchar(100),
    pathname     text,
    lastmodified text,
    filesize     bigint,
    etag         text,
    updatedate   timestamp with time zone
);

create index idx_cacheitem_reponame_pathname
    on public.cacheitem (reponame, pathname);

create index idx_cacheitem_reponame
    on public.cacheitem (reponame);

create table public.repomap
(
    rowid    bigserial
        constraint repomap_pk
            primary key,
    reponame varchar(100)
        constraint unique_repomap_reponame
            unique,
    baseurl  text
);
